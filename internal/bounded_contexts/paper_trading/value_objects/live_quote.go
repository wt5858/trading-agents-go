package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// LiveQuote 是给持仓估值用的最新报价。
//
// 它刻意只有三个字段，而不是复用 stock 上下文那个十几列的 Quote：
// 模拟交易只需要「这只票现在多少钱、这个价格是什么时候的」。
// 端口的入参出参越窄，两个限界上下文之间的耦合面就越小——
// stock 上下文以后给 Quote 加一列财务指标，不该让模拟盘重新编译。
//
// Price 用 decimal 而不是 float64：报价要直接参与市值与浮动盈亏的乘法，
// 在这里降级成 float64，等于把浮点误差从数据源一路带进账户估值。
type LiveQuote struct {
	Code  shared_vo.StockCode
	Price decimal.Decimal
	AsOf  time.Time
}

func NewLiveQuote(code shared_vo.StockCode, price decimal.Decimal, asOf time.Time) LiveQuote {
	return LiveQuote{Code: code, Price: RoundMoney(price), AsOf: asOf}
}

// Usable 判定这条报价能不能用于估值。
//
// 价格为 0 不是「免费」，而是「没拿到数据」（新股未上市、数据源当天没同步）。
// 拿 0 去估值会让整个组合的市值凭空归零，看板上就是一次假的爆仓，
// 所以估值路径必须先问一句能不能用，再决定要不要回退到成本价。
func (q LiveQuote) Usable() bool {
	return !q.Code.IsZero() && q.Price.GreaterThan(decimal.Zero)
}
