package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// ReferencePrice 是「加入自选那一刻的参考价」。
//
// 它是一个**被观测到的事实**，不是派生量：加入自选时行情里的最新价是多少，
// 就原样存多少，之后永不改写。没有它的话，「自选以来涨了多少」这个数字就无从谈起——
// 总不能每次打开页面都拿今天的价和今天的价去比。
//
// TradeDate 一起存下来，是为了让这个基准可审计：用户看到 +12.3% 时，
// 必须能回答「相对哪一天的哪个价」。只存价格不存日期的话，
// 停牌、除权之后这个数字会变得无法解释，也无法核对。
type ReferencePrice struct {
	Price     decimal.Decimal
	TradeDate shared_vo.TradeDate
	At        time.Time
}

// NewReferencePrice 构造参考价。
//
// 价格 <= 0 一律视为「没有参考价」而不是错误：加自选时行情源可能正好不可用，
// 或者标的当天停牌。为此让「添加自选股」整个失败是本末倒置——
// 自选股的核心价值是那条记录本身，参考价只是锦上添花。
func NewReferencePrice(price decimal.Decimal, tradeDate shared_vo.TradeDate, at time.Time) ReferencePrice {
	if !price.IsPositive() {
		return ReferencePrice{}
	}
	if at.IsZero() {
		at = time.Now()
	}
	return ReferencePrice{Price: price, TradeDate: tradeDate, At: at}
}

// RehydrateReferencePrice 从数据库重建，不做任何判定。
func RehydrateReferencePrice(price decimal.Decimal, tradeDate shared_vo.TradeDate, at *time.Time) ReferencePrice {
	rp := ReferencePrice{Price: price, TradeDate: tradeDate}
	if at != nil {
		rp.At = *at
	}
	return rp
}

func (r ReferencePrice) IsZero() bool { return !r.Price.IsPositive() }

// GainPctSince 算「自加入自选以来的涨跌幅」，返回 ok=false 表示算不出来。
//
// ===========================================================================
// 这是本上下文唯一一个在读路径上做除法的地方，必须说清楚它为什么合法
// ===========================================================================
//
// 本服务的通则是：派生的乘除结果在写路径算一次、随聚合落库，读路径直接取，
// 绝不重算（见 analysis.Batch.Percent、scheduling 的 success_rate）。
// 那条规则针对的是「两个操作数都已经定格」的派生量——它们重算一次就多一种口径。
//
// 这里的情况不同，而且差别是本质的：
//
//   - 分母 ReferencePrice.Price 是写路径固化的事实，落库后永不改写；
//   - 分子是**实时行情**，它压根没有、也不该有一份持久化的副本。
//     真要把它落库，就等于要求每有一个报价跳动就把所有自选行重写一遍。
//
// 也就是说，这个百分比没有「写入时刻」可言，它是一次性的读时比较。
// 把它算在这里（领域层）而不是散在 handler 里，是为了让口径只有一份。
//
// 请注意与它相邻的另一个数字的区别：行情行上的**当日涨跌幅**
// （QuoteSnapshot.ChangePct）绝不在这里用 (Price-PreClose)/PreClose 现算，
// 它是数据源给出、由 stock 上下文落库的值，本上下文只负责读回来显示。
// 一个现算、一个照搬，两者在响应里是两个不同的字段，不允许互相替代。
func (r ReferencePrice) GainPctSince(currentPrice decimal.Decimal) (decimal.Decimal, bool) {
	if r.IsZero() || !currentPrice.IsPositive() {
		return decimal.Zero, false
	}
	// 分母已由 IsZero 保证为正，除法不会 panic——这一点在 decimal 下比在
	// float64 下更要紧：浮点除零得到 Inf 还能一路传下去，decimal 除零直接崩。
	return decimalx.PercentChange(currentPrice, r.Price), true
}
