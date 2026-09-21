package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// Trade 是一条只追加的成交记录。
//
// # 它既不是聚合根，也不是「被加载的子实体」
//
// 它由 PaperAccount.Buy / Sell 产生，挂在 account.NewTrades 上，
// 和账户、持仓在同一个事务里落库——所以它没有自己的仓储，也不需要乐观锁。
// 但它**不会**在加载账户时被读回来：成交历史无上限增长，
// 把它做成常驻子实体会让「下一单」的加载成本随历史线性膨胀。
// 读历史走 PaperAccountRepository.ListTrades，返回的是值对象 TradeRecord。
//
// # 每一个乘除派生量都在这里被固化
//
// Amount（= 数量 × 价格）、Fee、RealizedPnL 在成交那一刻算一次，写进这条记录，
// 之后任何读路径都只搬运、不重算。这条记录就是「当时到底发生了什么」的唯一凭证。
type Trade struct {
	ID        string
	AccountID string
	Code      shared_vo.StockCode
	Side      value_objects.OrderSide

	Quantity decimal.Decimal
	Price    decimal.Decimal

	// Amount 是成交金额：落库的乘法派生量。
	Amount decimal.Decimal
	Fee    decimal.Decimal
	// RealizedPnL 只在卖出时非零：= (卖价 − 落库的平均成本) × 数量 − 手续费。
	RealizedPnL decimal.Decimal
	// CashAfter 是成交后的现金快照，让历史可以逐笔对账。
	CashAfter decimal.Decimal

	TradedAt time.Time
}

// ToRecord 把成交记录转成读模型。
//
// Amount 是**原样搬运**的。这里不写 t.Quantity.Mul(t.Price)，
// 因为真正影响了现金余额的是 t.Amount 这个存量值；重算出来的数字
// 在取整口径不同的时候会和余额变动对不上，而对不平的账本毫无价值。
func (t *Trade) ToRecord() value_objects.TradeRecord {
	return value_objects.RehydrateTradeRecord(
		t.ID, t.AccountID, t.Code, t.Side,
		t.Quantity, t.Price, t.Amount, t.Fee, t.RealizedPnL, t.CashAfter,
		t.TradedAt,
	)
}
