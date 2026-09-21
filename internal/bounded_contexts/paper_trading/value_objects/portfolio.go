package value_objects

import (
	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// PositionValuation 是单个持仓在某一时刻的估值读模型。
//
// # 这里是全上下文唯一允许「读时现算乘除」的地方，原因必须写清楚
//
// 规则是「乘除派生量算一次、落库、读路径只读存量」。MarketValue 与 UnrealizedPnL
// 是这条规则的**真例外**，不是疏漏：
//
//   - 它们的输入里有一个实时报价。报价每一秒都在变，因此根本不存在
//     「当时的那个事实」可以落库——存下来的任何一个浮动盈亏，在下一秒就是错的。
//   - 相比之下 AvgCost / CostBasis / Amount 的输入全部在成交那一刻就固定了，
//     所以它们必须落库。
//
// 请不要把这两个字段「优化」成数据库列。如果哪天真要做浮动盈亏的历史曲线，
// 正确的做法是另起一张按日快照表（那是另一个事实：某日收盘的浮动盈亏），
// 而不是在 paper_positions 上加两列随报价漂移的脏数据。
//
// 注意这个例外只放宽了「实时报价那一半」：算式的另一半 AvgCost 用的是
// **落库的存量值**，绝不在这里用 CostBasis / Quantity 重新推一遍平均成本。
type PositionValuation struct {
	Code     shared_vo.StockCode
	Quantity decimal.Decimal

	// AvgCost / CostBasis 是成交时算好并落库的存量值，本结构只是把它们搬运出来。
	AvgCost   decimal.Decimal
	CostBasis decimal.Decimal

	// MarketPrice 是估值用价格。没有可用报价时回退为 AvgCost，见 HasQuote。
	MarketPrice decimal.Decimal
	// MarketValue = Quantity × MarketPrice，读时计算（依赖实时报价）。
	MarketValue decimal.Decimal
	// UnrealizedPnL = (MarketPrice − 落库的 AvgCost) × Quantity，读时计算。
	UnrealizedPnL decimal.Decimal

	// HasQuote 为 false 表示这只票当前没有可用报价，估值已回退到成本价。
	// 必须把这个事实透出去：一个「浮动盈亏 0.00」的持仓，
	// 到底是真的不赚不亏，还是行情没同步上，使用者有权知道。
	HasQuote bool
}

// ValuePosition 用一条实时报价给持仓估值。
//
// avgCost 与 costBasis 必须由调用方从聚合里取落库值传进来，本函数不反推。
func ValuePosition(
	code shared_vo.StockCode,
	quantity, avgCost, costBasis decimal.Decimal,
	quote LiveQuote,
) PositionValuation {
	price := avgCost
	hasQuote := false
	if quote.Usable() {
		price = quote.Price
		hasQuote = true
	}

	return PositionValuation{
		Code:      code,
		Quantity:  quantity,
		AvgCost:   avgCost,
		CostBasis: costBasis,

		MarketPrice: price,
		MarketValue: RoundMoney(quantity.Mul(price)),
		// 没有报价时价格回退成 AvgCost，浮动盈亏自然算出 0——
		// 这正是想要的语义：拿不到行情就不编造盈亏。
		UnrealizedPnL: RoundMoney(price.Sub(avgCost).Mul(quantity)),
		HasQuote:      hasQuote,
	}
}

// PortfolioSummary 是账户组合的估值汇总读模型。
type PortfolioSummary struct {
	AccountID string

	// Cash / InitialCash / RealizedPnL 全部来自聚合的落库值。
	// 尤其 RealizedPnL：它是每一笔卖出成交时算好累加进去的，
	// 绝不在这里遍历成交历史重新累加——那会随着历史增长越算越慢，
	// 而且一旦历史被分页截断就静默算错。
	Cash        decimal.Decimal
	InitialCash decimal.Decimal
	RealizedPnL decimal.Decimal

	PositionCount int
	// TotalCost 是持仓成本合计，由落库的 CostBasis 相加而来（只有加法，无乘除）。
	TotalCost decimal.Decimal
	// MarketValue / TotalValue / UnrealizedPnL 依赖实时报价，读时计算。
	MarketValue   decimal.Decimal
	TotalValue    decimal.Decimal
	UnrealizedPnL decimal.Decimal

	Positions []PositionValuation
}

// NewPortfolioSummary 汇总组合估值。
func NewPortfolioSummary(
	accountID string,
	cash, initialCash, realizedPnL decimal.Decimal,
	positions []PositionValuation,
) PortfolioSummary {
	totalCost := decimal.Zero
	marketValue := decimal.Zero
	unrealized := decimal.Zero
	for _, p := range positions {
		totalCost = totalCost.Add(p.CostBasis)
		marketValue = marketValue.Add(p.MarketValue)
		unrealized = unrealized.Add(p.UnrealizedPnL)
	}
	if positions == nil {
		positions = []PositionValuation{}
	}

	return PortfolioSummary{
		AccountID:     accountID,
		Cash:          cash,
		InitialCash:   initialCash,
		RealizedPnL:   realizedPnL,
		PositionCount: len(positions),
		TotalCost:     RoundMoney(totalCost),
		MarketValue:   RoundMoney(marketValue),
		TotalValue:    RoundMoney(cash.Add(marketValue)),
		UnrealizedPnL: RoundMoney(unrealized),
		Positions:     positions,
	}
}

// TotalPnL 是总盈亏（已实现 + 浮动）。纯加法，读时计算无害。
func (s PortfolioSummary) TotalPnL() decimal.Decimal {
	return s.RealizedPnL.Add(s.UnrealizedPnL)
}
