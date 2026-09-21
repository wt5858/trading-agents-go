// Package dtos 是报告上下文的持久化形状：表结构、JSON 列格式，以及与聚合的双向映射。
//
// 映射写在 DTO 文件里（DTO 上挂 ToDomain()，包级函数 FromDomainXxx()），不另开 mapper 包：
// 映射与它服务的表结构必须同生共死，拆开只会让改一次列要动两个目录，还容易漏。
//
// 铁律：DTO 绝不越过 repositories/ 这一层。上层拿到的永远是聚合或值对象。
package dtos

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 编译期断言：DTO 必须自带表名，漏写会让 GORM 按结构体名推导出错误的表。
var _ interface{ TableName() string } = ReportDto{}

// ReportDto 是 analysis_reports 表的持久化对象。
//
// 通用约定与本服务其它上下文一致：时间统一 datetime(3)；时间戳由聚合自己维护，
// 因此关掉 GORM 的 autoCreateTime——否则「回填历史报告」会被 GORM 静默改写成今天。
type ReportDto struct {
	// 报告 ID 由领域服务生成（带前缀的业务 ID），不用自增：
	// 报告是通过领域事件异步生成的，事件处理器需要一个不依赖数据库往返的稳定标识。
	ID string `gorm:"column:id;type:varchar(40);primaryKey"`

	// task_id 上的唯一索引是本上下文最重要的一条约束，它同时干两件事：
	//
	//  1. 表达不变式：一次分析只该产出一份报告。同一个任务出现第二份报告，
	//     意味着任务被重复消费了——那是一个必须暴露出来的 bug，不是可以静默覆盖的小事。
	//  2. 提供幂等：领域事件是至少一次投递的。重放时第二次 INSERT 会撞唯一键，
	//     仓储把它翻成 AlreadyExists，事件处理器据此认定「已经做过了」。
	//     这让整条链路不需要任何「先查再插」——那种写法在并发重放下根本挡不住重复。
	TaskID string `gorm:"column:task_id;type:varchar(40);not null;uniqueIndex:uk_reports_task_id"`

	// (user_id, created_at) 覆盖唯一的热查询：用户的报告列表按时间倒序翻页。
	// 复合索引而不是两个单列索引：单列索引下 MySQL 只能用其一，
	// 剩下的排序要落到 filesort，报告数量一多翻页就会肉眼可见地变慢。
	UserID    uint64    `gorm:"column:user_id;not null;index:idx_reports_user_created,priority:1"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;index:idx_reports_user_created,priority:2,sort:desc;autoCreateTime:false"`

	// symbol / market / trade_date 是从聚合拍平出来的列，供运维排查与看板聚合
	// （「本月 600519 出了多少份报告」不该去 JSON 里捞）。
	Symbol    string `gorm:"column:symbol;type:varchar(16);not null;default:''"`
	Market    string `gorm:"column:market;type:varchar(8);not null;default:''"`
	SymbolRaw string `gorm:"column:symbol_raw;type:varchar(24);not null;default:''"`
	TradeDate string `gorm:"column:trade_date;type:char(10);not null;default:''"`

	Title   string `gorm:"column:title;type:varchar(128);not null;default:''"`
	Summary string `gorm:"column:summary;type:text"`

	// 章节整体读写，用 JSON 列而不是另开一张 report_sections 表。
	//
	// 章节是值对象：它没有独立身份，永远随报告整体产生、整体消失，也从不被单独更新。
	// 给它一张表就等于给了它一个它不该有的生命周期，还会让「读一份报告」
	// 从一条主键查询变成一次 join + 一次排序。
	Sections []byte `gorm:"column:sections;type:json"`

	// 终局数字落库存储，读路径直接用。它们在报告生成那一刻就是既成事实，
	// 任何形式的读时重算都会让同一份报告给出不同的数字。
	Action      string          `gorm:"column:action;type:varchar(16);not null;default:''"`
	Confidence  decimal.Decimal `gorm:"column:confidence;type:decimal(5,4);not null;default:0"`
	RiskScore   decimal.Decimal `gorm:"column:risk_score;type:decimal(4,2);not null;default:0"`
	TargetPrice decimal.Decimal `gorm:"column:target_price;type:decimal(18,4);not null;default:0"`
	StopLoss    decimal.Decimal `gorm:"column:stop_loss;type:decimal(18,4);not null;default:0"`
	Position    decimal.Decimal `gorm:"column:position;type:decimal(6,2);not null;default:0"`
}

func (ReportDto) TableName() string { return "analysis_reports" }

// ---------------------------------------------------------------------------
// JSON 列的显式格式
// ---------------------------------------------------------------------------

// sectionJSON 是 value_objects.Section 的落库形态。
//
// 不直接序列化值对象：Section 的字段名属于领域层，一旦领域改名或调整结构，
// 存量 JSON 就静默读不出来了。把存储格式钉死在这一层，正是映射必须和 DTO
// 同文件的理由。
type sectionJSON struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Order   int    `json:"order"`
}

// ---------------------------------------------------------------------------
// 聚合 <-> DTO
// ---------------------------------------------------------------------------

// FromDomainReport 把报告聚合投影成 DTO。
func FromDomainReport(r *entities.Report) *ReportDto {
	return &ReportDto{
		ID:     r.ID,
		TaskID: r.TaskID,
		UserID: r.UserID,

		Symbol:    r.Symbol.Symbol,
		Market:    string(r.Symbol.Market),
		SymbolRaw: r.Symbol.Raw,
		TradeDate: r.TradeDate.String(),

		Title:   r.Title,
		Summary: r.Summary,
		// Order 跟着章节一起落库：排序是生成那一刻固化的事实，读回来直接用。
		Sections: marshalSections(r.Sections),

		Action:      r.Action.String(),
		Confidence:  r.Confidence,
		RiskScore:   r.RiskScore,
		TargetPrice: r.TargetPrice,
		StopLoss:    r.StopLoss,
		Position:    r.Position,

		CreatedAt: r.CreatedAt,
	}
}

// ToDomain 把 DTO 重建成聚合根。
//
// 这里不做校验：库里的行是既成事实，用 VO 的校验构造器去解析它，会让一条历史脏数据
// 把整个报告列表接口打挂。校验属于写入路径。
//
// 同理，终局数字原样读回，不夹取、不重算。
func (dto ReportDto) ToDomain() *entities.Report {
	return &entities.Report{
		ID:     dto.ID,
		UserID: dto.UserID,
		TaskID: dto.TaskID,

		// 直接拼装 StockCode 而不是走 shared_vo.NewStockCode：写入时已经规范化过，
		// 读路径再跑一次校验，只会让「代码规则改过」的历史行整条查不出来。
		Symbol:    shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.SymbolRaw},
		TradeDate: shared_vo.MustTradeDate(dto.TradeDate),

		Title:    dto.Title,
		Summary:  dto.Summary,
		Sections: unmarshalSections(dto.Sections),

		Action:      analysis_vo.Action(dto.Action),
		Confidence:  dto.Confidence,
		RiskScore:   dto.RiskScore,
		TargetPrice: dto.TargetPrice,
		StopLoss:    dto.StopLoss,
		Position:    dto.Position,

		CreatedAt: dto.CreatedAt,
	}
}

func ToDomainReports(rows []ReportDto) []*entities.Report {
	out := make([]*entities.Report, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

func marshalSections(sections []value_objects.Section) []byte {
	if len(sections) == 0 {
		return nil
	}
	rows := make([]sectionJSON, 0, len(sections))
	for _, s := range sections {
		rows = append(rows, sectionJSON{
			Key:     s.Key.String(),
			Title:   s.Title,
			Content: s.Content,
			Order:   s.Order,
		})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		// 章节是纯数据，不含 chan/func，编码失败只可能是不可恢复的编程错误。
		// 退化成 NULL 列也好过让一次报告生成整体崩掉——报告的结论字段仍然可用。
		return nil
	}
	return b
}

// unmarshalSections 容忍 NULL 与损坏的 JSON：解析失败时返回空章节，
// 让调用方看到「一份没有正文的报告」，而不是让整个列表接口 500。
func unmarshalSections(raw []byte) []value_objects.Section {
	if len(raw) == 0 {
		return nil
	}
	var rows []sectionJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]value_objects.Section, 0, len(rows))
	for _, r := range rows {
		out = append(out, value_objects.Section{
			Key:     value_objects.SectionKey(r.Key),
			Title:   r.Title,
			Content: r.Content,
			Order:   r.Order,
		})
	}
	return out
}
