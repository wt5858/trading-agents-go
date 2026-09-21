// Package value_objects 提供股票上下文的值对象：校验通过、不可变、无生命周期。
//
// 本包放的是「读模型」——Quote / Kline / Financial / News / SocialPost。
// 它们没有身份也没有生命周期：同一只票同一交易日的行情被重抓一次，
// 新的那份就是全部事实，不存在「同一个 Quote 发生了变化」这种说法，
// 因此它们是值对象而不是实体，也因此不配备各自的仓储——
// 只有聚合根 Stock 有仓储，读模型由 MarketDataRepository 在读路径上直接返回。
//
// 这一层不出现 bson/gorm 标签：持久化格式的演进属于 repositories/，让 DTO 去承载。
package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Quote 是某一交易日的行情快照。
//
// Code / TradeDate 用值对象而不是裸 string：裸 string 的 symbol 在系统里同时出现过
// "600519"、"600519.SH"、"sh600519" 三种写法，交易日则有 YYYYMMDD 与 YYYY-MM-DD 两种，
// 格式错配只会在运行期表现为「查不到数据」，无处收敛。提升成 VO 后，
// 规范化与校验只发生在构造点。
//
// Change / ChangePct / Amount / Turnover 全部是数据源直接给出、随快照一起落库的值，
// 读路径只负责读回来。绝不在这里用 Close-PreClose 或 Close*Volume 现算：
// 各家源的复权口径、成交额统计口径彼此不同，现算的结果和源给的对不上，
// 而下游（涨停判定、换手率因子）分不清自己拿到的是哪一种。
//
// 价格与比率一律 decimal：PE、PB、Turnover 会直接进选股的 $gte 比较，
// 一个落在阈值边界上的值因为二进制浮点的末位误差被筛掉或漏进来，
// 事后没有任何线索可查——筛选结果不会报错，只会悄悄少一只票。
type Quote struct {
	Code      shared_vo.StockCode
	TradeDate shared_vo.TradeDate
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	PreClose  decimal.Decimal
	Change    decimal.Decimal
	ChangePct decimal.Decimal
	Volume    decimal.Decimal
	Amount    decimal.Decimal
	Turnover  decimal.Decimal
	PE        decimal.Decimal
	PB        decimal.Decimal
	Source    string
	UpdatedAt time.Time
}

// NewQuote 构造并校验行情快照。
//
// 只校验「自然键完整」这一条：价格为 0 在停牌日是合法的，
// 把它当错误会让停牌票的整条同步链路断掉。
func NewQuote(code shared_vo.StockCode, date shared_vo.TradeDate) (Quote, error) {
	if code.IsZero() {
		return Quote{}, custom_errors.Invalid("行情快照缺少股票代码")
	}
	if date.IsZero() {
		return Quote{}, custom_errors.Invalid("行情快照缺少交易日")
	}
	return Quote{Code: code, TradeDate: date}, nil
}

// HasNaturalKey 判定自然键 (symbol, trade_date) 是否完整。
// 仓储用它过滤掉会 upsert 出 symbol="" 垃圾文档的记录。
func (q Quote) HasNaturalKey() bool { return !q.Code.IsZero() && !q.TradeDate.IsZero() }

// Market 是访问器而非独立字段：市场已经包含在 StockCode 里，
// 单独存一份迟早会和 Code.Market 不一致。
func (q Quote) Market() shared_vo.Market { return q.Code.Market }

func (q Quote) Symbol() string { return q.Code.Symbol }

// IsLimitUp 粗略判定涨停，用于风控提示。
// 判定读的是落库的 ChangePct，而不是现场用 Close/PreClose 反推——
// 停牌复牌、除权日这些场景下两者并不相等，以数据源口径为准。
func (q Quote) IsLimitUp() bool {
	if q.Code.Market != shared_vo.MarketCN {
		return false
	}
	return q.ChangePct.GreaterThanOrEqual(limitUpThresholdPct)
}

// limitUpThresholdPct 是涨停判定阈值（9.8%）。写成包级常量而不是字面量，
// 是因为 decimal 的字面量必须经过构造函数，每次调用都 NewFromFloat 一遍
// 既浪费也容易把 9.8 写成 float 字面量再引入一次浮点转换。
var limitUpThresholdPct = decimal.RequireFromString("9.8")
