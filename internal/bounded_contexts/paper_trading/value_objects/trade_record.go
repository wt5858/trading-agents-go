package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// TradeRecord 是成交历史的读模型值对象。
//
// # 为什么成交历史是值对象而不是聚合里的子实体
//
// 成交记录只追加、永不修改，而且数量无上限：一个跑了一年的模拟账户可能有上万条。
// 把它做成 PaperAccount 的子实体，意味着「买一手」这个动作要先把整部历史
// 加载进内存——聚合的加载成本会随使用时长线性膨胀，直到下单超时。
// 所以写路径只把「本次工作单元新产生的成交」挂在聚合上（PaperAccount.NewTrades），
// 读路径则由仓储直接分页返回本值对象。
//
// 这条读路径上出现 DTO → VO 的直接映射是允许的：读模型没有生命周期、
// 没有不变式需要守，让它绕道聚合只会白白付出加载整个账户的代价。
//
// # Amount / Fee / RealizedPnL 都是落库的存量值
//
// 尤其是 Amount：它就是当时从现金里划走（或划入）的那个数。
// 渲染时**绝不能**写 Quantity.Mul(Price) 重算——那是另一个数字，
// 只要取整口径有一点差异，用户看到的成交额就和余额的变动对不上，
// 而账本对不平的模拟盘没有任何参考价值。
type TradeRecord struct {
	ID        string
	AccountID string
	Code      shared_vo.StockCode
	Side      OrderSide

	Quantity decimal.Decimal
	Price    decimal.Decimal

	// Amount 是成交金额（落库的乘法派生量，读路径直接取）。
	Amount decimal.Decimal
	// Fee 是本次成交的手续费（落库）。
	Fee decimal.Decimal
	// RealizedPnL 是本次成交实现的盈亏（落库）。
	// 买入恒为 0：买入不实现盈亏，只是把现金换成持仓。
	RealizedPnL decimal.Decimal
	// CashAfter 是成交后的现金余额快照。存它是为了让历史可对账：
	// 顺着历史把每一行的 CashAfter 串起来，就能定位账本在哪一笔开始分叉。
	CashAfter decimal.Decimal

	TradedAt time.Time
}

// RehydrateTradeRecord 从持久化行重建读模型。
//
// 注意签名里 amount 是一个**入参**而不是由 quantity × price 现算出来的：
// 这正是「派生量算一次、落库、读路径只读存量」的落点。把它改成现算，
// 就等于宣布库里那一列是可以被忽略的——那它一开始就不该存在。
func RehydrateTradeRecord(
	id, accountID string,
	code shared_vo.StockCode,
	side OrderSide,
	quantity, price, amount, fee, realizedPnL, cashAfter decimal.Decimal,
	tradedAt time.Time,
) TradeRecord {
	return TradeRecord{
		ID:          id,
		AccountID:   accountID,
		Code:        code,
		Side:        side,
		Quantity:    quantity,
		Price:       price,
		Amount:      amount,
		Fee:         fee,
		RealizedPnL: realizedPnL,
		CashAfter:   cashAfter,
		TradedAt:    tradedAt,
	}
}

// NetCashFlow 返回这笔成交对现金的净影响：买入为负，卖出为正。
// 它是由两个存量值（Amount、Fee）加减得来的，不涉及乘除，
// 因此可以安全地在读路径上算——加减法不会引入新的取整口径。
func (r TradeRecord) NetCashFlow() decimal.Decimal {
	if r.Side.IsBuy() {
		return r.Amount.Add(r.Fee).Neg()
	}
	return r.Amount.Sub(r.Fee)
}
