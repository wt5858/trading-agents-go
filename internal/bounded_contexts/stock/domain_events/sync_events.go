package domain_events

import (
	"encoding/json"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	OnSyncCompletedEventName = "stock.sync_completed"
	OnSyncFailedEventName    = "stock.sync_failed"
)

// OnSyncCompleted 在一次同步结束（全部成功或部分成功）后发布。
//
// 携带 SuccessRate 的存量值而不是让消费方自己用 Succeeded/Total 去除：
// 成功率是乘除派生值，落库那一刻算出来的才是事实，消费方重算会和库里对不上。
//
// SuccessRate 在事件里是字符串而不是 JSON 数字：事件会被序列化落到日志、
// 消息队列与下游存储，中途任何一个消费者用 float64 反序列化就会把精度丢掉，
// 而事件一旦发出去就再也没有机会纠正。
type OnSyncCompleted struct {
	domain_event.BaseDomainEvent
	RunID       string `json:"runId"`
	Kind        string `json:"kind"`
	Market      string `json:"market"`
	Status      string `json:"status"`
	Total       int    `json:"total"`
	Succeeded   int    `json:"succeeded"`
	Failed      int    `json:"failed"`
	SuccessRate string `json:"successRate"`
}

func NewOnSyncCompleted(runID, kind, market, status string, total, succeeded, failed int, successRate decimal.Decimal) *OnSyncCompleted {
	return &OnSyncCompleted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		RunID:           runID,
		Kind:            kind,
		Market:          market,
		Status:          status,
		Total:           total,
		Succeeded:       succeeded,
		Failed:          failed,
		SuccessRate:     decimalx.FormatPercent(successRate),
	}
}

func (e *OnSyncCompleted) Name() string { return OnSyncCompletedEventName }

func (e *OnSyncCompleted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnSyncFailed 在同步整体失败后发布，供告警消费。
type OnSyncFailed struct {
	domain_event.BaseDomainEvent
	RunID  string `json:"runId"`
	Kind   string `json:"kind"`
	Market string `json:"market"`
	Reason string `json:"reason"`
}

func NewOnSyncFailed(runID, kind, market, reason string) *OnSyncFailed {
	return &OnSyncFailed{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		RunID:           runID,
		Kind:            kind,
		Market:          market,
		Reason:          reason,
	}
}

func (e *OnSyncFailed) Name() string { return OnSyncFailedEventName }

func (e *OnSyncFailed) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
