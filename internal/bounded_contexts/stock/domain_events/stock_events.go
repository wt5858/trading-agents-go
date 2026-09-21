package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

// 本上下文只登记退市/恢复上市两个事件。
//
// 刻意没有「上市」与「资料更新」：它们是每日全量同步的常规产物，一次同步产出几千条，
// 而消费方（分析任务调度、自选股提醒）真正需要响应的只有「这只票不再可分析」。
const (
	OnStockDelistedEventName = "stock.delisted"
	OnStockRelistedEventName = "stock.relisted"
)

// OnStockDelisted 在标的被标记退市后发布。
//
// StockName 而不是 Name：Name() 是 DomainEvent 接口的方法名，同名字段会把它顶掉。
type OnStockDelisted struct {
	domain_event.BaseDomainEvent
	StockID   uint64 `json:"stockId"`
	Symbol    string `json:"symbol"`
	Market    string `json:"market"`
	StockName string `json:"stockName"`
}

func NewOnStockDelisted(stockID uint64, symbol, market, stockName string) *OnStockDelisted {
	return &OnStockDelisted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		StockID:         stockID,
		Symbol:          symbol,
		Market:          market,
		StockName:       stockName,
	}
}

func (e *OnStockDelisted) Name() string { return OnStockDelistedEventName }

func (e *OnStockDelisted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnStockRelisted 在误报退市被撤销后发布，消费方据此把该标的重新纳入调度。
type OnStockRelisted struct {
	domain_event.BaseDomainEvent
	StockID uint64 `json:"stockId"`
	Symbol  string `json:"symbol"`
	Market  string `json:"market"`
}

func NewOnStockRelisted(stockID uint64, symbol, market string) *OnStockRelisted {
	return &OnStockRelisted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		StockID:         stockID,
		Symbol:          symbol,
		Market:          market,
	}
}

func (e *OnStockRelisted) Name() string { return OnStockRelistedEventName }

func (e *OnStockRelisted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
