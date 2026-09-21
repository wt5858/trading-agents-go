package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// QuoteSnapshot 是自选股列表所需的最小行情视图。
//
// # 为什么本上下文自己定义它，而不是直接用 stock 上下文的 Quote
//
// 端口由消费方声明，端口上的类型也就必须由消费方拥有。直接引用
// stock/value_objects.Quote 会让自选股在编译期依赖股票上下文的完整读模型：
// 那边加一个换手率字段、改一次 Kline 的形状，这边就得跟着重新编译，
// 而自选股列表其实只要「最新价 + 当日涨跌幅」两个数。
//
// 字段刻意只有四个。多一个字段就多一份要在 DI 适配层维护的映射，
// 而自选股看板需要的信息就这么多。
//
// # ChangePct 是照搬来的，不是算出来的
//
// 当日涨跌幅由数据源给出、由 stock 上下文落库，本上下文原样读回来显示。
// 绝不在任何一层用 (Price - PreClose) / PreClose 现算：各家源的复权口径不同，
// 现算的结果和源给的对不上，而用户看到的两个数字打架时无从解释。
// 与之相对的「自选以来涨跌幅」则必须现算，理由见 ReferencePrice.GainPctSince。
type QuoteSnapshot struct {
	Code      shared_vo.StockCode
	Price     decimal.Decimal // 最新价
	ChangePct decimal.Decimal // 当日涨跌幅（%），数据源口径，不重算
	TradeDate shared_vo.TradeDate
	UpdatedAt time.Time
}

func (q QuoteSnapshot) IsZero() bool { return q.Code.IsZero() }
