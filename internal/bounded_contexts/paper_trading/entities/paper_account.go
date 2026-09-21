// Package entities 承载模拟交易上下文的聚合。
//
// # 这是一个模拟账户，不是真实交易
//
// 这里没有真实资金、没有券商通道、没有撮合。它存在的目的是学习与回测：
// 用户拿一笔虚拟资金按历史或实时价格记账，观察自己的策略表现。
// 因此本包产出的任何盈亏数字都只是模拟结果，不构成任何投资建议，
// 接口层对外渲染时必须带上免责声明（见 application/http_handlers）。
//
// # 聚合边界
//
//	PaperAccount（根）
//	  ├── Positions []*Position   子实体：有界、只经由根到达、与现金共享不变式
//	  └── NewTrades []*Trade      本次工作单元新产生的成交，随根一起落库
//
// 成交历史不是被加载的子实体（无上限增长），读历史走仓储的读路径。
package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// maxPositions 是单账户的持仓标的数上限。
//
// 它存在的理由不是业务规则，而是聚合边界的自我保护：Position 之所以能当子实体，
// 前提就是它「有界」。没有这个上限，一个脚本刷出几万只持仓，
// 加载账户就会变成一次全表扫描，子实体的设计前提当场失效。
const maxPositions = 200

// PaperAccount 是模拟交易上下文的聚合根。
type PaperAccount struct {
	domain_event.EventRecorder

	ID     string
	UserID uint64
	Name   string

	// InitialCash 是开户资金，也是重置时要恢复到的基准，落库后不再变更。
	InitialCash decimal.Decimal
	// Cash 是可用现金。它和 Positions 共同构成账本的两端，
	// 任何一次成交都必须同时改动这两端，这正是它们必须同属一个聚合的原因。
	Cash decimal.Decimal
	// RealizedPnL 是已实现盈亏累计，**每笔卖出时算一次并累加落库**。
	// 绝不在读路径上遍历成交历史重新累加：历史会分页、会增长，重算既慢又会算错。
	RealizedPnL decimal.Decimal
	// TotalFee 是累计手续费，同样是逐笔累加的落库值。
	// 单独存一份是因为「不算手续费我本来能赚多少」是模拟盘最常被问的问题之一，
	// 而从分页的历史里去 SUM 一遍既慢又和账户余额对不上。
	TotalFee decimal.Decimal

	// Positions 是持仓子实体集合，只能由根的方法增删改。
	Positions []*Position

	// NewTrades 是**本次工作单元**新产生的成交，不是全部历史。
	//
	// 仓储的 Save 会把它们和账户、持仓写进同一个事务，然后清空。
	// 这样做的意义是：现金变动与它的凭证要么一起提交、要么一起回滚，
	// 不会出现「钱扣了但没有成交记录」这种无法解释的账本。
	NewTrades []*Trade

	CreatedAt time.Time
	UpdatedAt time.Time

	// Version 是乐观锁版本号。
	//
	// 同一个账户上两笔并发下单如果各自「读 - 改 - 写」，后写的那次会用
	// 自己读到的旧现金覆盖前一次的结果，凭空多出一笔钱。把版本号写进
	// UPDATE 的 WHERE 里，检查与写入就成了一个原子操作。详见仓储的 Save。
	Version int64
}

// OpenPaperAccount 开立模拟账户。这是账户进入系统的唯一入口。
func OpenPaperAccount(id string, userID uint64, name string, initialCash decimal.Decimal) (*PaperAccount, error) {
	if id == "" {
		return nil, custom_errors.Invalid("模拟账户 ID 不能为空")
	}
	if userID == 0 {
		return nil, custom_errors.Invalid("模拟账户必须归属于一个用户")
	}
	if name == "" {
		name = "模拟账户"
	}
	if !initialCash.GreaterThan(decimal.Zero) {
		return nil, custom_errors.Invalid("初始资金必须大于 0")
	}

	now := time.Now()
	cash := value_objects.RoundMoney(initialCash)
	return &PaperAccount{
		ID:          id,
		UserID:      userID,
		Name:        name,
		InitialCash: cash,
		Cash:        cash,
		RealizedPnL: decimal.Zero,
		TotalFee:    decimal.Zero,
		Positions:   []*Position{},
		CreatedAt:   now,
		UpdatedAt:   now,
		Version:     0,
	}, nil
}

// PositionOf 按股票代码取持仓，没有则返回 nil。
// 这是外界访问子实体的唯一入口——Position 不可被独立查询，只能经由根到达。
func (a *PaperAccount) PositionOf(code shared_vo.StockCode) *Position {
	for _, p := range a.Positions {
		if p.Code.Symbol == code.Symbol && p.Code.Market == code.Market {
			return p
		}
	}
	return nil
}

// Buy 买入。
//
// # 校验与变更在同一个方法里完成，这是刻意的
//
// 本方法不提供、也永远不会提供一个配套的 CanBuy()。「先问能不能买、再买」
// 是教科书式的 TOCTOU：两个并发请求都能在同一瞬间得到「现金够」的答复，
// 然后双双扣款，账户现金变成负数。把「够不够」和「扣多少」锁在同一个方法里，
// 单个聚合实例上就不存在这个窗口；跨请求的那一层则由仓储的乐观锁兜住。
//
// 派生量的固化点也在这里：amount = 数量 × 价格 算一次，写进 Trade，
// 同时它（连同手续费）就是从 Cash 里真正划走的数。读路径以后只认这个数。
func (a *PaperAccount) Buy(code shared_vo.StockCode, quantity, price, fee decimal.Decimal) (*Trade, error) {
	if err := validateOrderInput(code, quantity, price, fee); err != nil {
		return nil, err
	}

	quantity = value_objects.RoundQuantity(quantity)
	price = value_objects.RoundMoney(price)
	fee = value_objects.RoundMoney(fee)

	// 乘法派生量：只在这里算这一次。
	amount := value_objects.RoundMoney(quantity.Mul(price))
	totalCost := value_objects.RoundMoney(amount.Add(fee))

	// 不变式与变更之间没有任何间隙。
	if totalCost.GreaterThan(a.Cash) {
		return nil, custom_errors.Conflict(
			"可用资金不足：本次买入需要 %s，账户可用 %s",
			value_objects.FormatMoney(totalCost), value_objects.FormatMoney(a.Cash))
	}

	pos := a.PositionOf(code)
	if pos == nil && len(a.Positions) >= maxPositions {
		return nil, custom_errors.Conflict("单个模拟账户最多持有 %d 只标的", maxPositions)
	}

	now := time.Now()
	a.Cash = value_objects.RoundMoney(a.Cash.Sub(totalCost))
	a.TotalFee = value_objects.RoundMoney(a.TotalFee.Add(fee))

	if pos == nil {
		// 建仓：平均成本就是含费单位成本，同样算一次就固化。
		avgCost := value_objects.RoundMoney(totalCost.Div(quantity))
		a.Positions = append(a.Positions, newPosition(code, quantity, avgCost, totalCost, now))
	} else {
		pos.applyBuy(quantity, totalCost, now)
	}

	return a.recordTrade(code, value_objects.SideBuy, quantity, price, amount, fee, decimal.Zero, now), nil
}

// Sell 卖出。
//
// 与 Buy 同理，「持仓够不够」和「减多少」是同一个方法里的同一件事，
// 不存在 CanSell() 这种给 TOCTOU 开门的接口。
//
// 已实现盈亏 = (卖价 − **落库的**平均成本) × 数量 − 手续费。
// 注意算式里的平均成本取的是 pos.AvgCost 这个存量值，不是现场用
// CostBasis / Quantity 反推出来的——反推会引入取整差异，
// 于是同一笔卖出在不同代码路径上算出不同的盈亏。
func (a *PaperAccount) Sell(code shared_vo.StockCode, quantity, price, fee decimal.Decimal) (*Trade, error) {
	if err := validateOrderInput(code, quantity, price, fee); err != nil {
		return nil, err
	}

	quantity = value_objects.RoundQuantity(quantity)
	price = value_objects.RoundMoney(price)
	fee = value_objects.RoundMoney(fee)

	pos := a.PositionOf(code)
	if pos == nil {
		return nil, custom_errors.Conflict("账户未持有 %s，无法卖出", code.FullSymbol())
	}
	if quantity.GreaterThan(pos.Quantity) {
		return nil, custom_errors.Conflict(
			"可卖数量不足：%s 持有 %s，本次卖出 %s",
			code.FullSymbol(),
			value_objects.FormatQuantity(pos.Quantity),
			value_objects.FormatQuantity(quantity))
	}

	amount := value_objects.RoundMoney(quantity.Mul(price))
	if fee.GreaterThan(amount) {
		// 手续费吃掉全部成交金额意味着这笔卖出净流出现金。模拟盘里这必然是
		// 参数填错，直接拒绝比让现金余额莫名减少更有帮助。
		return nil, custom_errors.Invalid(
			"手续费 %s 超过成交金额 %s", value_objects.FormatMoney(fee), value_objects.FormatMoney(amount))
	}

	now := time.Now()
	proceeds := value_objects.RoundMoney(amount.Sub(fee))
	// 已实现盈亏：乘除派生量，算一次，写进成交记录并累加进账户。
	realized := value_objects.RoundMoney(price.Sub(pos.AvgCost).Mul(quantity)).Sub(fee)
	// 按落库的平均成本等比释放成本基数。
	releasedCost := value_objects.RoundMoney(pos.AvgCost.Mul(quantity))

	a.Cash = value_objects.RoundMoney(a.Cash.Add(proceeds))
	a.RealizedPnL = value_objects.RoundMoney(a.RealizedPnL.Add(realized))
	a.TotalFee = value_objects.RoundMoney(a.TotalFee.Add(fee))

	pos.applySell(quantity, releasedCost, now)
	if pos.IsEmpty() {
		// 清仓即移除。留一行数量为 0 的持仓会污染持仓数统计，
		// 也会让「我现在持有哪些票」这个问题给出错误答案。
		a.removePosition(code)
	}

	return a.recordTrade(code, value_objects.SideSell, quantity, price, amount, fee, realized, now), nil
}

// Reset 把账户恢复到开户状态。
//
// # 为什么不删除成交历史
//
// 成交记录是只追加的审计数据。允许某个业务动作批量删除它，就等于承认
// 「历史是可以被改写的」，那它作为对账凭证的价值也就没有了。
// 代价是重置之后，历史里的盈亏总和会和账户上的 RealizedPnL 对不上——
// 这个断层通过 OnPaperAccountReset 事件里的 DiscardedRealizedPnL 显式广播出去，
// 下游据此把收益曲线切段，而不是把它当成一个 bug 去「修」。
func (a *PaperAccount) Reset() error {
	now := time.Now()
	cleared := len(a.Positions)
	discarded := a.RealizedPnL

	a.Cash = a.InitialCash
	a.Positions = []*Position{}
	a.RealizedPnL = decimal.Zero
	a.TotalFee = decimal.Zero
	a.UpdatedAt = now

	a.AddDomainEvent(domain_events.NewOnPaperAccountReset(
		a.ID, a.UserID,
		value_objects.FormatMoney(a.InitialCash),
		cleared,
		value_objects.FormatMoney(discarded),
		now,
	))
	return nil
}

// Rename 改名。
func (a *PaperAccount) Rename(name string) error {
	if name == "" {
		return custom_errors.Invalid("账户名称不能为空")
	}
	a.Name = name
	a.UpdatedAt = time.Now()
	return nil
}

// TakeNewTrades 取走本次工作单元产生的成交并清空。
// 由仓储在事务内调用：取走即代表「这批成交的落库责任已经转移」，
// 和 EventRecorder 的排空语义一致，保证同一笔成交不会被写两次。
func (a *PaperAccount) TakeNewTrades() []*Trade {
	if len(a.NewTrades) == 0 {
		return nil
	}
	out := a.NewTrades
	a.NewTrades = nil
	return out
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// recordTrade 固化一笔成交并广播事件。
//
// amount / fee / realized 都由调用方算好传进来——本方法只负责记录，
// 绝不在这里重新乘一遍。派生量的计算点必须唯一。
func (a *PaperAccount) recordTrade(
	code shared_vo.StockCode,
	side value_objects.OrderSide,
	quantity, price, amount, fee, realized decimal.Decimal,
	at time.Time,
) *Trade {
	a.UpdatedAt = at
	t := &Trade{
		// 成交 ID 在聚合内部生成：它必须在事务提交前就存在
		// （事件里要带上它），而自增主键那时候还拿不到。
		ID:          idx.Prefixed("ptrade"),
		AccountID:   a.ID,
		Code:        code,
		Side:        side,
		Quantity:    quantity,
		Price:       price,
		Amount:      amount,
		Fee:         fee,
		RealizedPnL: realized,
		CashAfter:   a.Cash,
		TradedAt:    at,
	}
	a.NewTrades = append(a.NewTrades, t)

	a.AddDomainEvent(domain_events.NewOnPaperOrderExecuted(
		a.ID, a.UserID, t.ID,
		code.FullSymbol(), code.Market.String(), side.String(),
		value_objects.FormatQuantity(quantity),
		value_objects.FormatMoney(price),
		value_objects.FormatMoney(amount),
		value_objects.FormatMoney(fee),
		value_objects.FormatMoney(realized),
		value_objects.FormatMoney(a.Cash),
	))
	return t
}

func (a *PaperAccount) removePosition(code shared_vo.StockCode) {
	out := a.Positions[:0]
	for _, p := range a.Positions {
		if p.Code.Symbol == code.Symbol && p.Code.Market == code.Market {
			continue
		}
		out = append(out, p)
	}
	a.Positions = out
}

// validateOrderInput 是买卖共用的形状校验。
// 它抛 Invalid（参数本身不合法），而资金/持仓不足抛 Conflict（参数合法但当前状态不允许）——
// 这个区分让接口层能把前者映射成 400、后者映射成 409，调用方据此决定是改参数还是重试。
func validateOrderInput(code shared_vo.StockCode, quantity, price, fee decimal.Decimal) error {
	if code.IsZero() {
		return custom_errors.Invalid("股票代码不能为空")
	}
	if !quantity.GreaterThan(decimal.Zero) {
		return custom_errors.Invalid("委托数量必须大于 0")
	}
	if !price.GreaterThan(decimal.Zero) {
		return custom_errors.Invalid("委托价格必须大于 0")
	}
	if fee.LessThan(decimal.Zero) {
		return custom_errors.Invalid("手续费不能为负数")
	}
	return nil
}
