// Package domain_events 是模拟交易上下文对外广播的事实。
//
// 事件里的金额一律用字符串承载而不是 float64：事件会被序列化成 JSON 落到日志、
// 消息队列、下游存储，中途任何一个消费者用 float64 反序列化就会把精度丢掉。
// 用 decimal 的定长字符串表示，跨进程传输后仍然是同一个数。
package domain_events

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnPaperOrderExecutedEventName = "paper_trading.order_executed"
	OnPaperAccountResetEventName  = "paper_trading.account_reset"
)

// OnPaperOrderExecuted 表示一笔模拟成交已经在聚合内部完成。
//
// 它携带的全部是**落库的存量值**（Amount / Fee / RealizedPnL / CashAfter），
// 消费者不需要、也不应该用 Quantity × Price 去反推成交金额：
// 那样算出来的数和账户现金的实际变动可能对不上。
type OnPaperOrderExecuted struct {
	domain_event.BaseDomainEvent
	AccountID string `json:"accountId"`
	UserID    uint64 `json:"userId"`
	TradeID   string `json:"tradeId"`
	Symbol    string `json:"symbol"`
	Market    string `json:"market"`
	Side      string `json:"side"`

	Quantity    string `json:"quantity"`
	Price       string `json:"price"`
	Amount      string `json:"amount"`
	Fee         string `json:"fee"`
	RealizedPnL string `json:"realizedPnl"`
	CashAfter   string `json:"cashAfter"`

	// Simulated 恒为 true。它是一个显式的标记，而不是靠事件名里的
	// "paper_trading" 前缀去暗示：下游做通知、做报表的代码只要漏看一眼前缀，
	// 就可能把模拟成交混进真实业绩里对外展示。
	Simulated bool `json:"simulated"`
}

func NewOnPaperOrderExecuted(
	accountID string,
	userID uint64,
	tradeID, symbol, market, side string,
	quantity, price, amount, fee, realizedPnL, cashAfter string,
) *OnPaperOrderExecuted {
	return &OnPaperOrderExecuted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		AccountID:       accountID,
		UserID:          userID,
		TradeID:         tradeID,
		Symbol:          symbol,
		Market:          market,
		Side:            side,
		Quantity:        quantity,
		Price:           price,
		Amount:          amount,
		Fee:             fee,
		RealizedPnL:     realizedPnL,
		CashAfter:       cashAfter,
		Simulated:       true,
	}
}

func (e *OnPaperOrderExecuted) Name() string { return OnPaperOrderExecutedEventName }

func (e *OnPaperOrderExecuted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnPaperAccountReset 表示账户被重置回初始资金。
//
// 携带 ResetAt 而不是让消费者读 OccurredAt：重置是一条业务时间线上的分界点，
// 下游画收益曲线时要用它把曲线切段，这个语义值得有自己的字段。
type OnPaperAccountReset struct {
	domain_event.BaseDomainEvent
	AccountID string `json:"accountId"`
	UserID    uint64 `json:"userId"`
	// InitialCash 是重置后恢复到的初始资金。
	InitialCash string `json:"initialCash"`
	// ClearedPositions 是被清掉的持仓数量，用于下游核对。
	ClearedPositions int `json:"clearedPositions"`
	// DiscardedRealizedPnL 是被归零的已实现盈亏。
	// 它被显式带出来，是因为重置**不会**删除成交历史（历史是只追加的审计数据），
	// 于是重置之后「历史里的盈亏总和」和「账户上的已实现盈亏」必然对不上。
	// 把差额广播出去，下游才有办法解释这个断层，而不是当成 bug。
	DiscardedRealizedPnL string    `json:"discardedRealizedPnl"`
	ResetAt              time.Time `json:"resetAt"`
	Simulated            bool      `json:"simulated"`
}

func NewOnPaperAccountReset(
	accountID string,
	userID uint64,
	initialCash string,
	clearedPositions int,
	discardedRealizedPnL string,
	resetAt time.Time,
) *OnPaperAccountReset {
	return &OnPaperAccountReset{
		BaseDomainEvent:      domain_event.NewBaseDomainEvent(),
		AccountID:            accountID,
		UserID:               userID,
		InitialCash:          initialCash,
		ClearedPositions:     clearedPositions,
		DiscardedRealizedPnL: discardedRealizedPnL,
		ResetAt:              resetAt,
		Simulated:            true,
	}
}

func (e *OnPaperAccountReset) Name() string { return OnPaperAccountResetEventName }

func (e *OnPaperAccountReset) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
