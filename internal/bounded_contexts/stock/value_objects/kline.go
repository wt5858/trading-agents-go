package value_objects

import (
	"strings"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Period 是 K 线周期值对象。
type Period string

const (
	PeriodDaily   Period = "daily"
	PeriodWeekly  Period = "weekly"
	PeriodMonthly Period = "monthly"
)

// NewPeriod 解析周期。空串退化为日线——「没指定周期」在查询语境下的自然含义就是日线，
// 但非法值必须报错：静默降级会让一次前端拼写错误变成「图画出来了但不是要的那条」。
func NewPeriod(s string) (Period, error) {
	switch Period(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return PeriodDaily, nil
	case PeriodDaily:
		return PeriodDaily, nil
	case PeriodWeekly:
		return PeriodWeekly, nil
	case PeriodMonthly:
		return PeriodMonthly, nil
	}
	return "", custom_errors.Invalid("不支持的 K 线周期: %s", s)
}

func (p Period) Valid() bool {
	return p == PeriodDaily || p == PeriodWeekly || p == PeriodMonthly
}

func (p Period) String() string { return string(p) }

// OrDaily 在周期非法时退化为日线，供仓储查询这类「宁可给默认值也不要空结果」的路径使用。
func (p Period) OrDaily() Period {
	if p.Valid() {
		return p
	}
	return PeriodDaily
}

// Kline 是一根 K 线。
//
// Amount（成交额）与 Volume 一样，是数据源给什么就存什么的独立字段。
// Finnhub 这类不返回成交额的源宁可留 0，也不用 Close*Volume 估一个出来：
// 估出来的数字会被下游当成真实成交额去算量价背离，比缺数据更危险。
//
// 价量一律 decimal：K 线是全部技术指标的输入，均线与 MACD 是长序列的连加连乘，
// 浮点误差会随窗口长度单调累积，最后以「同一根 K 线在不同周期下算出不同均线」
// 的形式暴露出来，而那时已经无从判断差的是数据还是公式。
type Kline struct {
	Code      shared_vo.StockCode
	Period    Period
	TradeDate shared_vo.TradeDate
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	Amount    decimal.Decimal
	// Adjusted 标记是否已做复权。它必须跟着数据走：
	// 未复权价和前复权价混在同一条序列里会让所有技术指标失真。
	Adjusted bool
	Source   string
}

// HasNaturalKey 判定自然键 (symbol, period, trade_date) 是否完整。
// period 必须进键：同一天的日线和周线是两条合法记录。
func (k Kline) HasNaturalKey() bool {
	return !k.Code.IsZero() && !k.TradeDate.IsZero() && k.Period.Valid()
}

func (k Kline) Market() shared_vo.Market { return k.Code.Market }

func (k Kline) Symbol() string { return k.Code.Symbol }

// OHLCValid 校验蜡烛图不变式 low <= min(open,close) <= max(open,close) <= high。
// 违反它的数据画出来会是「上影线长在实体下方」，ATR/KDJ 也会算出负数。
func (k Kline) OHLCValid() bool {
	if k.High.LessThan(k.Low) {
		return false
	}
	lo, hi := k.Open, k.Close
	if lo.GreaterThan(hi) {
		lo, hi = hi, lo
	}
	return k.Low.LessThanOrEqual(lo) && hi.LessThanOrEqual(k.High)
}
