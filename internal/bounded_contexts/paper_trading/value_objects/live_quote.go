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
//
// 刻意不含新鲜度：估值可以用一条旧报价（配合 HasQuote 把事实透出去就行），
// 下单不行。两条路径的判据不同，所以拆成两个方法，见 Fresh。
func (q LiveQuote) Usable() bool {
	return !q.Code.IsZero() && q.Price.GreaterThan(decimal.Zero)
}

// MaxQuoteAge 是下单时还肯认的报价年龄上限。
//
// 取两周，是在「挡住真正的陈旧数据」和「不误伤正常休市」之间找的位置：
// 行情是日线，AsOf 是交易日，所以周末、小长假天然就会让最新报价有几天的年龄；
// A 股的春节和国庆能连休 9 天，阈值低于它就会在每年两个固定时段集体拒单。
// 而这个检查真正要挡的是「数据源断了几个月没人发现」——两周足够把它抓出来。
//
// 这个值是按 A 股休市日历定的。要是以后接入的市场休市更长，改这里，
// 别在调用点各写各的。
const MaxQuoteAge = 14 * 24 * time.Hour

// Fresh 判定这条报价新不新，够不够用来撮合。
//
// 为什么下单必须问这一句：市价单是「按当前价成交」，而 resolvePrice 只要拿到
// 一条 Usable 的报价就直接当成交价用。数据源停更之后，那条报价仍然 Usable——
// 它只是很旧。于是用户以为自己按今天的价格买入，实际成交在几个月前的收盘价上，
// 而且这个错误没有任何提示，只会沉淀成一笔成本离谱的持仓，污染此后全部盈亏。
func (q LiveQuote) Fresh(now time.Time, maxAge time.Duration) bool {
	if q.AsOf.IsZero() {
		// 没有时间戳就无法判断新鲜度。这种报价不该用于撮合——
		// 「不知道多旧」和「很旧」在下单这件事上是同一回事。
		return false
	}
	return !q.AsOf.Before(now.Add(-maxAge))
}
