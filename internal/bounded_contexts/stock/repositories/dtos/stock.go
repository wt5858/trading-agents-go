// Package dtos 承载股票上下文的持久化对象与领域映射。
//
// 映射函数（ToDomain / FromDomainXxx）刻意与 DTO 定义在同一文件，本上下文没有
// mapper 包：表结构和它的映射规则是同一件事的两面，拆开只会让改一列要开两个文件。
//
// 本包的类型绝不允许越过 repositories/ 向上泄漏——带 gorm/bson 标签的结构一旦
// 出现在 domain_services 或 handler 的签名里，换存储就要改业务代码。
package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// StockDto 对应 stocks 表。
//
// 它与聚合根刻意分离：聚合可以自由重构字段，而表结构的演进受迁移约束。
//
// 通用约定与 identity 保持一致：时间统一 datetime(3)、关掉 GORM 的自动时间戳
// （时间由聚合根维护，否则「导入存量数据」会被静默改写）、金额类一律 decimal。
type StockDto struct {
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// (market, symbol) 才是业务主键：symbol 单列不唯一（港股 00700 与潜在的同号标的），
	// 且同步任务按市场分批跑，market 放在联合索引首位可以让「某市场全量列表」走索引。
	Market string `gorm:"column:market;type:varchar(8);not null;uniqueIndex:uk_stocks_market_symbol,priority:1"`
	Symbol string `gorm:"column:symbol;type:varchar(16);not null;uniqueIndex:uk_stocks_market_symbol,priority:2"`
	// Raw 保留数据源的原始写法（600519.SH），排查数据源差异时有用，不参与索引。
	Raw string `gorm:"column:raw_code;type:varchar(24);not null;default:''"`
	// name / industry 单列索引服务于「按名称搜索」和「按行业筛选」两条高频路径。
	// name 上的 LIKE '关键词%' 前缀查询能吃到索引；LIKE '%关键词%' 吃不到，
	// 这是刻意的取舍——A 股全量不到 6000 行，中缀搜索全表扫也在毫秒级。
	Name     string     `gorm:"column:name;type:varchar(64);not null;default:'';index:idx_stocks_name"`
	Industry string     `gorm:"column:industry;type:varchar(64);not null;default:'';index:idx_stocks_industry"`
	Area     string     `gorm:"column:area;type:varchar(64);not null;default:''"`
	ListDate *time.Time `gorm:"column:list_date;type:date"`
	Delisted bool       `gorm:"column:delisted;not null;default:false"`
	// 市值单位统一为元（换算在各数据源里完成），落库用定点数保证跨数据源对账可复现：
	// float 列在 SQL 层做聚合会累积误差。市值是数据源直接给出的派生量，
	// 只在这里存一次、读回来用，任何地方都不允许再用股价乘股本反推。
	//
	// decimal(20,4) 的容量：A 股最大市值约 2e12 元、美股约 4e12 美元，
	// 距离 1e16 的上限还有四个数量级的余量。
	TotalMV   decimal.Decimal `gorm:"column:total_mv;type:decimal(20,4);not null;default:0"`
	CircMV    decimal.Decimal `gorm:"column:circ_mv;type:decimal(20,4);not null;default:0"`
	Source    string          `gorm:"column:source;type:varchar(16);not null;default:''"`
	UpdatedAt time.Time       `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (StockDto) TableName() string { return "stocks" }

// ToDomain 把 DTO 重建成聚合根。
//
// 这里直接拼装 StockCode 而不走 shared_vo.NewStockCode：库里的代码在写入时
// 已经规范化过，读路径再跑一次校验，只会让「数据源改过格式」的历史行整条查不出来。
// 校验属于写入路径。
func (dto StockDto) ToDomain() *entities.Stock {
	return &entities.Stock{
		ID:        dto.ID,
		Code:      shared_vo.StockCode{Symbol: dto.Symbol, Market: shared_vo.Market(dto.Market), Raw: dto.Raw},
		Name:      dto.Name,
		Industry:  dto.Industry,
		Area:      dto.Area,
		ListDate:  dto.ListDate,
		Delisted:  dto.Delisted,
		TotalMV:   dto.TotalMV,
		CircMV:    dto.CircMV,
		Source:    dto.Source,
		UpdatedAt: dto.UpdatedAt,
	}
}

// FromDomainStock 把聚合根投影成 DTO。
//
// StockCode 被拆成三列而不是整体序列化：market / symbol 要进唯一索引和 WHERE 条件，
// 存成 JSON 就查不了了。
func FromDomainStock(s *entities.Stock) *StockDto {
	return &StockDto{
		ID:        s.ID,
		Market:    string(s.Code.Market),
		Symbol:    s.Code.Symbol,
		Raw:       s.Code.Raw,
		Name:      s.Name,
		Industry:  s.Industry,
		Area:      s.Area,
		ListDate:  s.ListDate,
		Delisted:  s.Delisted,
		TotalMV:   s.TotalMV,
		CircMV:    s.CircMV,
		Source:    s.Source,
		UpdatedAt: s.UpdatedAt,
	}
}

func ToDomainStocks(rows []StockDto) []*entities.Stock {
	out := make([]*entities.Stock, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
