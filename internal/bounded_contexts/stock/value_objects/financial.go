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
	// AnnounceDate 是这份财报**公开披露**的日期，与 ReportDate 是两个完全不同的概念。
	//
	// ReportDate 是报告期：2023-12-31 那份年报覆盖的是 2023 年，
	// 而它实际要到 2024 年 4 月底才对外公布。做历史回测时，只有 AnnounceDate
	// 说得清「这份数据在那一天到底存不存在」——按 ReportDate 过滤，
	// 会让回测 2024-03-01 的模型看到一个月后才发布的年报，
	// 而这种未来函数不会报错，只会让结论准得可疑。
	//
	// 可能为零值：老数据在这个字段加上之前就已入库，且并非每家源都提供它。
	// 零值时的处理见 DisclosedBy——那里不允许「不知道就放行」。
	AnnounceDate shared_vo.TradeDate
	PeriodType   PeriodType
	Revenue      decimal.Decimal
	NetProfit    decimal.Decimal
	EPS          decimal.Decimal
	PE           decimal.Decimal
	PB           decimal.Decimal
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

// disclosureLagDays 是 AnnounceDate 缺失时的保守回退：报告期之后多少天才算已披露。
//
// 取值依据是 A 股的法定披露期限上限：年报与一季报均为次年 4 月 30 日前，
// 半年报 8 月 31 日前，三季报 10 月 31 日前。换算成自报告期起的天数，
// 年报最长 120 天，其余最长 62 天（半年报 06-30 → 08-31）。
//
// 用**上限**而不是实际中位数，是因为这里的两类错误代价极不对称：
// 放进一份尚未披露的财报，整批回测结论就是错的且无从察觉；
// 而多滤掉几份早已披露的财报，只是让那次分析少一块素材——
// 提示词里会如实写明「财务数据缺失」，模型不会因此编造。
const (
	disclosureLagAnnualDays = 120
	disclosureLagOtherDays  = 62
)

// DisclosedBy 判定这份财报在 asOf 当天是否已经公开披露。
//
// asOf 为零值表示实时分析（请求没带交易日），此时一切已入库的数据都是「现在」，
// 不做任何过滤——回测才需要这道闸门，实时场景加上它只会平白丢数据。
//
// AnnounceDate 缺失时**不放行**，而是退到按报告期加保守滞后期估算。
// 「不知道披露日就当它已经披露」是这里最危险的写法：现有历史数据全部没有这个字段，
// 那样写等于这道防护对存量数据完全不生效，而它看起来是生效的。
func (f Financial) DisclosedBy(asOf shared_vo.TradeDate) bool {
	if asOf.IsZero() {
		return true
	}
	if !f.AnnounceDate.IsZero() {
		return f.AnnounceDate.String() <= asOf.String()
	}
	if f.ReportDate.IsZero() {
		// 连报告期都没有的记录无从判断，宁可不用。
		return false
	}
	lag := disclosureLagOtherDays
	if f.PeriodType == PeriodTypeAnnual {
		lag = disclosureLagAnnualDays
	}
	return f.ReportDate.AddDays(lag).String() <= asOf.String()
}

func (f Financial) Market() shared_vo.Market { return f.Code.Market }

func (f Financial) Symbol() string { return f.Code.Symbol }
