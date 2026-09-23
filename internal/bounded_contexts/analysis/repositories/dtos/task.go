// Package dtos 是分析上下文的持久化形状：表结构、JSON 列格式，以及与聚合的双向映射。
//
// 映射写在 DTO 文件里（DTO 上挂 ToDomain()，包级函数 FromDomainXxx()），
// 不另开 mapper 包：映射与它服务的表结构必须同生共死，拆开只会让改一次列
// 要动两个目录，还容易漏。
//
// 铁律：DTO 绝不越过 repositories/ 这一层。上层拿到的永远是聚合或值对象。
package dtos

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 编译期断言：DTO 必须自带表名，漏写会让 GORM 按结构体名推导出错误的表。
var (
	_ interface{ TableName() string } = TaskDto{}
	_ interface{ TableName() string } = BatchDto{}
)

// TaskDto 是 analysis_tasks 表的持久化对象。
//
// 它与聚合根刻意分离：聚合可以自由重构字段，而表结构的演进受迁移约束。
//
// 通用约定：时间统一 datetime(3)（毫秒足够任务编排，又避免 datetime(6) 的精度漂移）；
// 时间戳由聚合自己维护，因此关掉 GORM 的 autoCreateTime/autoUpdateTime——
// 否则「重放历史任务」「导入存量数据」这类场景会被 GORM 静默改写。
type TaskDto struct {
	// 任务 ID 由领域服务生成（UUID），不用自增：任务要先落库再入队列，
	// 队列里必须有一个稳定标识，自增主键在入队那一刻还拿不到。
	ID string `gorm:"column:id;type:varchar(40);primaryKey"`
	// 用户任务列表有两种形态（带状态筛选 / 不带），各配一条索引，两条都把排序列
	// created_at DESC, id DESC 包到了尾部，翻页不需要额外排序。
	// 不带状态时必须另有一条，因为 status 不是常量，复合索引里它后面的 created_at
	// 只是分段有序的，排不了序。
	// 单用户并发额度统计（CountRunningByUser）走 idx_tasks_user_status_created 的前两列。
	UserID uint64 `gorm:"column:user_id;not null;index:idx_tasks_user_status_created,priority:1;index:idx_tasks_user_created,priority:1"`
	Status string `gorm:"column:status;type:varchar(16);not null;index:idx_tasks_user_status_created,priority:2;index:idx_tasks_stale,priority:1"`
	// 批量任务的子任务靠 batch_id 聚合，单列索引即可（批次内任务数是十级别的）。
	// 它只是一个跨聚合的标识引用，没有也不该有外键约束——
	// 两个聚合各自落库，用外键就等于要求它们同事务写入。
	BatchID string `gorm:"column:batch_id;type:varchar(40);not null;default:'';index:idx_tasks_batch"`

	// 下面三列是从 request JSON 里拍平出来的冗余列，只为运维排查与看板聚合服务
	// （「今天 600519 被分析了多少次」这类查询不该去 JSON 里捞）。
	// 注意：request 列才是权威数据源，重建聚合时一律从 JSON 反序列化，绝不读这三列。
	Symbol    string `gorm:"column:symbol;type:varchar(16);not null;default:''"`
	Market    string `gorm:"column:market;type:varchar(8);not null;default:''"`
	TradeDate string `gorm:"column:trade_date;type:char(10);not null;default:''"`

	// Request / Progress / Result 都是整体读写的值对象，用 JSON 列。
	// 尤其 Progress 的步骤数组会随深度变化，拆表会变成一张写放大严重的小表。
	Request  []byte `gorm:"column:request;type:json"`
	Progress []byte `gorm:"column:progress;type:json"`
	// Result 仅在 status=completed 时有值，其余状态为 NULL。
	Result []byte `gorm:"column:result;type:json"`
	Error  string `gorm:"column:error;type:text"`

	Attempts int `gorm:"column:attempts;not null;default:0"`
	// state_changed_at 与 (status) 组成 idx_tasks_stale，专供停滞巡检。
	// 它每次状态迁移都刷新，和 started_at（首次启动、重试不覆盖）不是一回事。
	StateChangedAt time.Time `gorm:"column:state_changed_at;type:datetime(3);not null;index:idx_tasks_stale,priority:2"`
	// created_at 单列索引支撑「最近任务」排序与按时间归档的清理作业；
	// 另外两条复合索引把它当作排序尾巴（sort:desc 与查询的 ORDER BY 方向一致）。
	CreatedAt  time.Time  `gorm:"column:created_at;type:datetime(3);not null;index:idx_tasks_created_at;index:idx_tasks_user_status_created,priority:3,sort:desc;index:idx_tasks_user_created,priority:2,sort:desc;autoCreateTime:false"`
	StartedAt  *time.Time `gorm:"column:started_at;type:datetime(3)"`
	FinishedAt *time.Time `gorm:"column:finished_at;type:datetime(3)"`
}

func (TaskDto) TableName() string { return "analysis_tasks" }

// ---------------------------------------------------------------------------
// JSON 列的显式格式
// ---------------------------------------------------------------------------

// requestJSON 是 value_objects.Request 的落库形态。
//
// 不直接序列化值对象：shared_vo.StockCode / TradeDate 的内部表示属于领域层，
// 一旦领域改字段名或把字段私有化，存量 JSON 就静默读不出来了。
// 显式 DTO 把存储格式钉死在这一层，这正是映射必须和 DTO 同文件的理由。
type requestJSON struct {
	Symbol    string   `json:"symbol"`
	Market    string   `json:"market"`
	Raw       string   `json:"raw"`
	TradeDate string   `json:"trade_date"`
	Depth     int      `json:"depth"`
	Analysts  []string `json:"analysts"`
	LLMModel  string   `json:"llm_model"`
}

// resultJSON 是 value_objects.Result 的落库形态，理由同上。
type resultJSON struct {
	Symbol    string                       `json:"symbol"`
	Market    string                       `json:"market"`
	Raw       string                       `json:"raw"`
	TradeDate string                       `json:"trade_date"`
	Decision  value_objects.Decision       `json:"decision"`
	Reports   map[string]string            `json:"reports"`
	Usage     value_objects.TokenUsage     `json:"usage"`
	Phases    []value_objects.PhaseOutcome `json:"phases"`
	CreatedAt time.Time                    `json:"created_at"`
}

// ---------------------------------------------------------------------------
// 聚合 <-> DTO
// ---------------------------------------------------------------------------

// FromDomainTask 把任务聚合投影成 DTO。
func FromDomainTask(t *entities.Task) *TaskDto {
	req := t.Request
	return &TaskDto{
		ID:      t.ID,
		UserID:  t.UserID,
		Status:  t.Status.String(),
		BatchID: t.BatchID,
		// 冗余列跟着 request 一起写，永远由 request 派生，不接受外部单独赋值。
		Symbol:    req.Code.Symbol,
		Market:    string(req.Code.Market),
		TradeDate: req.TradeDate.String(),
		Request:   marshalJSON(toRequestJSON(req)),
		// Progress 连同它已经算好的 Percent / ETASeconds 一起落库：
		// 派生量是快照的一部分，读回来直接用，不重算。
		Progress:       marshalJSON(t.Progress),
		Result:         marshalResult(t.Result),
		Error:          t.ErrMsg,
		Attempts:       t.Attempts,
		StateChangedAt: t.StateChangedAt,
		CreatedAt:      t.CreatedAt,
		StartedAt:      t.StartedAt,
		FinishedAt:     t.FinishedAt,
	}
}

func FromDomainTasks(tasks []*entities.Task) []*TaskDto {
	out := make([]*TaskDto, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, FromDomainTask(t))
	}
	return out
}

// ToDomain 把 DTO 重建成聚合根。
//
// 这里不做校验：数据库里的数据是既成事实，用 VO 的校验构造器去解析它，
// 会让一条历史脏数据把整个列表接口打挂。校验属于写入路径。
//
// Progress 也是整份读回，包括 Percent / ETASeconds：它们是写入当时固化的事实，
// 在读路径上重算会让同一条记录每刷新一次就给出不同的数字。
func (dto TaskDto) ToDomain() *entities.Task {
	var progress value_objects.Progress
	unmarshalJSON(dto.Progress, &progress)
	return &entities.Task{
		ID:         dto.ID,
		UserID:     dto.UserID,
		BatchID:    dto.BatchID,
		Request:    requestFromJSON(dto.Request),
		Status:     value_objects.Status(dto.Status),
		Progress:   progress,
		Result:     resultFromJSON(dto.Result),
		ErrMsg:     dto.Error,
		Attempts:   dto.Attempts,
		CreatedAt:  dto.CreatedAt,
		StartedAt:  dto.StartedAt,
		FinishedAt: dto.FinishedAt,
		// 直接赋值而不经由 setStatus：ToDomain 是在重建一个既成事实，
		// 不是在发起一次状态迁移，刷新时间戳会把库里的停滞信息抹掉。
		StateChangedAt: dto.StateChangedAt,
	}
}

func ToDomainTasks(rows []TaskDto) []*entities.Task {
	out := make([]*entities.Task, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}

func toRequestJSON(r value_objects.Request) requestJSON {
	return requestJSON{
		Symbol:    r.Code.Symbol,
		Market:    string(r.Code.Market),
		Raw:       r.Code.Raw,
		TradeDate: r.TradeDate.String(),
		Depth:     r.Depth.Int(),
		Analysts:  r.Analysts,
		LLMModel:  r.LLMModel,
	}
}

func requestFromJSON(raw []byte) value_objects.Request {
	var dto requestJSON
	unmarshalJSON(raw, &dto)
	// 直接拼装 StockCode 而不是走 shared_vo.NewStockCode：写入时已经规范化过，
	// 读路径再跑一次校验，只会让「数据源改过格式」的历史行整条查不出来。
	code := shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.Raw}
	return value_objects.RehydrateRequest(
		code,
		shared_vo.MustTradeDate(dto.TradeDate),
		value_objects.Depth(dto.Depth),
		dto.Analysts,
		dto.LLMModel,
	)
}

// marshalResult 对 nil 结果返回 nil，让列保持 NULL。
// 未完成的任务占了在库任务的大头，写 "null" 字符串会白白撑大表空间。
func marshalResult(r *value_objects.Result) []byte {
	if r == nil {
		return nil
	}
	return marshalJSON(resultJSON{
		Symbol:    r.Code.Symbol,
		Market:    string(r.Code.Market),
		Raw:       r.Code.Raw,
		TradeDate: r.TradeDate.String(),
		Decision:  r.Decision,
		Reports:   r.Reports,
		Usage:     r.Usage,
		Phases:    r.Phases,
		CreatedAt: r.CreatedAt,
	})
}

func resultFromJSON(raw []byte) *value_objects.Result {
	if len(raw) == 0 {
		return nil
	}
	var dto resultJSON
	if err := json.Unmarshal(raw, &dto); err != nil {
		// 结果列损坏时返回 nil 而不是报错：任务的状态与进度仍然是可用信息，
		// 让整条查询失败等于把一次数据问题放大成接口不可用。
		return nil
	}
	return &value_objects.Result{
		Code:      shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.Raw},
		TradeDate: shared_vo.MustTradeDate(dto.TradeDate),
		Decision:  dto.Decision,
		Reports:   dto.Reports,
		Usage:     dto.Usage,
		Phases:    dto.Phases,
		CreatedAt: dto.CreatedAt,
	}
}

// marshalJSON 把值对象编码进 json 列。失败时返回 nil 而不是报错：
// 这些结构全部由领域层构造，不含 chan/func，编码失败只可能是不可恢复的编程错误，
// 让它退化成 NULL 列也好过让一次分析任务因为序列化崩掉。
func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// unmarshalJSON 容忍 NULL 与空串：老数据、手工修过的行都可能是空的，
// 解析失败时保持目标值为零值，上层看到的是「空进度」而不是查询失败。
func unmarshalJSON(raw []byte, dst any) {
	if len(raw) == 0 {
		return
	}
	_ = json.Unmarshal(raw, dst)
}
