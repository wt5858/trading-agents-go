package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// Position 是 PaperAccount 的**子实体**，不是聚合根。
//
// # 为什么它是子实体而不是独立聚合
//
// 三条判据同时成立：
//   - 数量有界：一个账户持有的标的个数是几十级别，加载整个持仓列表是常数级代价；
//   - 只能经由根到达：外界拿到它的唯一途径是 account.Positions / account.PositionOf；
//   - 它和现金余额共享同一条不变式——「买入扣的现金必须等于建仓增加的成本」。
//     这条不变式跨不了事务边界，所以持仓和现金必须在同一个聚合里、同一次提交中变更。
//
// 因此 Position **没有自己的仓储**。它的落库是 PaperAccountRepository.Save 的内部细节。
// 给它配一个 PositionRepository，就等于允许有人绕开账户直接改持仓，
// 那条「现金与持仓同增同减」的不变式立刻失效。
//
// # 为什么字段是导出的，而变更方法是不导出的
//
// 字段导出是为了让 repositories/dtos 能直接映射，不必为持久化开一堆 getter。
// 但 applyBuy / applySell 不导出：它们只能被同包的 PaperAccount 调用，
// 编译器因此替我们挡住了「在领域服务里直接改持仓数量」这种写法。
// 构造函数 newPosition 同理——持仓只能由根在一次买入中创建出来。
type Position struct {
	// Code 是跨聚合引用，用值对象承载而不是持有 stock.Stock 实体。
	// 持仓需要的只是「哪只票」这个标识；引一个别的聚合的实体进来，
	// 会让这两个聚合的生命周期绑死，加载持仓也会变成一次跨上下文的连表。
	Code shared_vo.StockCode

	Quantity decimal.Decimal

	// AvgCost 是移动加权平均成本，**乘除派生量，买入时算一次并随持仓落库**。
	// 卖出计算已实现盈亏时读的就是这个落库值。
	AvgCost decimal.Decimal

	// CostBasis 是当前持仓占用的总成本（含买入手续费），同样是落库的派生量。
	//
	// 它和 AvgCost 看似冗余（CostBasis ≈ AvgCost × Quantity），但两个都必须存：
	// AvgCost 按金额精度取整后，反乘回去未必等于当初实际划走的现金。
	// 存量的 CostBasis 才是「这笔持仓一共花了多少钱」的事实，
	// 组合汇总里的持仓成本合计读的是它，不是 AvgCost × Quantity 现算的结果。
	CostBasis decimal.Decimal

	OpenedAt  time.Time
	UpdatedAt time.Time
}

// newPosition 只能由 PaperAccount 在买入路径上调用。
func newPosition(code shared_vo.StockCode, quantity, avgCost, costBasis decimal.Decimal, at time.Time) *Position {
	return &Position{
		Code:      code,
		Quantity:  quantity,
		AvgCost:   avgCost,
		CostBasis: costBasis,
		OpenedAt:  at,
		UpdatedAt: at,
	}
}

// RehydratePosition 由 repositories/dtos 在重建聚合时调用。
//
// 它**不重算** AvgCost：库里那个值才是当时买入路径上算出来、并据此扣过现金的事实。
// 用 costBasis / quantity 在读路径上重新除一遍，会因为取整口径与历史公式的差异
// 得到一个略微不同的平均成本，于是同一笔持仓在不同版本的代码里算出不同的已实现盈亏。
// 这与 stock 上下文 RehydrateSyncStats 不重算成功率是同一条纪律。
func RehydratePosition(
	code shared_vo.StockCode,
	quantity, avgCost, costBasis decimal.Decimal,
	openedAt, updatedAt time.Time,
) *Position {
	return &Position{
		Code:      code,
		Quantity:  quantity,
		AvgCost:   avgCost,
		CostBasis: costBasis,
		OpenedAt:  openedAt,
		UpdatedAt: updatedAt,
	}
}

// Symbol 是持仓在聚合内部的局部标识：同一个账户里一只票只有一行持仓。
// 子实体的身份只需要在聚合内唯一，不需要全局 ID。
func (p *Position) Symbol() string { return p.Code.Symbol }

// applyBuy 加仓：数量累加，成本基数累加，平均成本**重新算一次并固化**。
//
// totalCost 含手续费。买入佣金计入成本基数是标准的加权平均成本法，
// 也让账本天然对平：从现金里划走多少，成本基数就增加多少。
func (p *Position) applyBuy(quantity, totalCost decimal.Decimal, at time.Time) {
	p.Quantity = value_objects.RoundQuantity(p.Quantity.Add(quantity))
	p.CostBasis = value_objects.RoundMoney(p.CostBasis.Add(totalCost))
	p.AvgCost = value_objects.RoundMoney(p.CostBasis.Div(p.Quantity))
	p.UpdatedAt = at
}

// applySell 减仓：数量与成本基数按**落库的 AvgCost** 等比释放，平均成本保持不变。
//
// 平均成本法下卖出不改变剩余持仓的单位成本，这一点必须守住：
// 如果这里顺手用剩余 CostBasis / 剩余 Quantity 重算一遍 AvgCost，
// 取整误差会在每次卖出时被放大一点，几十笔之后平均成本就会明显漂移。
func (p *Position) applySell(quantity, releasedCost decimal.Decimal, at time.Time) {
	p.Quantity = value_objects.RoundQuantity(p.Quantity.Sub(quantity))
	p.CostBasis = value_objects.RoundMoney(p.CostBasis.Sub(releasedCost))
	p.UpdatedAt = at
}

// IsEmpty 判定持仓是否已清空。清仓后这一行必须从聚合里移除，
// 而不是留一行 quantity = 0 的空持仓：空持仓会混进组合估值的持仓数统计，
// 也会让「我持有哪些票」这个最基本的问题给出错误答案。
func (p *Position) IsEmpty() bool { return !p.Quantity.GreaterThan(decimal.Zero) }
