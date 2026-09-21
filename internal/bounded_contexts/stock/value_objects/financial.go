package value_objects

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// PeriodType 是财报口径。
//
// 三种值并存是因为数据源口径本就不同：A 股按报告期给 annual/quarter，
// 美股的 Finnhub 只给 TTM 横截面。硬把 ttm 归到 quarter 会让同比计算算错，
// 所以把差异如实保留在类型里，由消费方决定怎么用。
type PeriodType string

const (
	PeriodTypeAnnual  PeriodType = "annual"
	PeriodTypeQuarter PeriodType = "quarter"
	PeriodTypeTTM     PeriodType = "ttm"
)

// NewPeriodType 解析财报口径。无法识别时归为季报而不是报错：
// 这是一个描述性标签，一个陌生的口径名不应该让整批财务数据同步失败。
func NewPeriodType(s string) PeriodType {
	switch PeriodType(strings.ToLower(strings.TrimSpace(s))) {
	case PeriodTypeAnnual:
		return PeriodTypeAnnual
	case PeriodTypeTTM:
		return PeriodTypeTTM
	default:
		return PeriodTypeQuarter
	}
}

func (p PeriodType) String() string { return string(p) }

// PeriodTypeOfReportDate 从报告期推断口径：12-31 是年报，其余是季报。
func PeriodTypeOfReportDate(d shared_vo.TradeDate) PeriodType {
	if c := d.Compact(); len(c) == 8 && c[4:] == "1231" {
		return PeriodTypeAnnual
	}
	return PeriodTypeQuarter
}

// Financial 是一期财务数据。
//
// ReportDate 用 TradeDate 而不是裸 string：报告期在各数据源里是 20231231 / 2023-12-31
// 两种写法，而 Mongo 的唯一索引建在 report_date 上，格式不统一会直接导致同一期财报
// 存出两条文档。
type Financial struct {
	Code       shared_vo.StockCode
	ReportDate shared_vo.TradeDate
	PeriodType PeriodType
	Revenue    decimal.Decimal
	NetProfit  decimal.Decimal
	EPS        decimal.Decimal
	PE         decimal.Decimal
	PB         decimal.Decimal
	// ROE / GrossMargin / NetMargin / DebtRatio 统一为百分数口径（12.34 表示 12.34%），
	// 各数据源的换算在 marketdata 层完成，领域层只认这一种口径。
	ROE         decimal.Decimal
	GrossMargin decimal.Decimal
	// NetMargin 是独立存储的净利率，不是读路径上用 NetProfit/Revenue 现算的结果。
	//
	// 理由有三：一是各家源本来就直接提供该指标（Tushare 的 netprofit_margin、
	// Finnhub 的 netProfitMarginTTM），现算等于把源的口径丢掉再造一个；
	// 二是 Tushare 的 fina_indicator 表根本没有稳定的绝对营收/净利字段，
	// Revenue/NetProfit 经常是 0，现算只会得到 0 这个假值；
	// 三是除法在营收为 0 时要额外兜 NaN，而 NaN 一旦进了聚合计算会污染整列结果。
	//
	// 改用 decimal 之后第三条理由更硬了：decimal 除零是 panic 而不是 NaN，
	// 现算的写法会把一次数据缺失变成一次进程崩溃。
	NetMargin decimal.Decimal
	DebtRatio decimal.Decimal
	Source    string
	UpdatedAt time.Time
}

// HasNaturalKey 判定自然键 (symbol, report_date) 是否完整。
func (f Financial) HasNaturalKey() bool { return !f.Code.IsZero() && !f.ReportDate.IsZero() }

func (f Financial) Market() shared_vo.Market { return f.Code.Market }

func (f Financial) Symbol() string { return f.Code.Symbol }
