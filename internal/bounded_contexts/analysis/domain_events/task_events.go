// Package domain_events 定义分析上下文对外广播的既成事实。
//
// 事件里刻意不带 *Task 指针或整份 Result：事件是「已经发生的事实的快照」，
// 带指针会让消费方读到聚合的后续变更，也会让事件无法被序列化投递到进程外。
package domain_events

import (
	"encoding/json"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	OnTaskQueuedEventName    = "analysis.task_queued"
	OnTaskStartedEventName   = "analysis.task_started"
	OnTaskCompletedEventName = "analysis.task_completed"
	OnTaskFailedEventName    = "analysis.task_failed"
	OnTaskCanceledEventName  = "analysis.task_canceled"
)

// OnTaskQueued 在任务进入排队态时发出。Requeue 区分首次提交与失败重试，
// 消费方（计费、配额）据此决定是否重复计数。
type OnTaskQueued struct {
	domain_event.BaseDomainEvent
	TaskID  string `json:"taskId"`
	UserID  uint64 `json:"userId"`
	BatchID string `json:"batchId,omitempty"`
	Symbol  string `json:"symbol"`
	Depth   int    `json:"depth"`
	Requeue bool   `json:"requeue"`
}

func NewOnTaskQueued(taskID string, userID uint64, batchID, symbol string, depth int, requeue bool) *OnTaskQueued {
	return &OnTaskQueued{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		TaskID:          taskID,
		UserID:          userID,
		BatchID:         batchID,
		Symbol:          symbol,
		Depth:           depth,
		Requeue:         requeue,
	}
}

func (e *OnTaskQueued) Name() string { return OnTaskQueuedEventName }

func (e *OnTaskQueued) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTaskStarted 在 worker 取到任务并开始执行时发出。
type OnTaskStarted struct {
	domain_event.BaseDomainEvent
	TaskID  string `json:"taskId"`
	UserID  uint64 `json:"userId"`
	Attempt int    `json:"attempt"`
}

func NewOnTaskStarted(taskID string, userID uint64, attempt int) *OnTaskStarted {
	return &OnTaskStarted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		TaskID:          taskID,
		UserID:          userID,
		Attempt:         attempt,
	}
}

func (e *OnTaskStarted) Name() string { return OnTaskStartedEventName }

func (e *OnTaskStarted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTaskCompleted 在任务成功产出结果时发出。
//
// 它同时是「批次结算」的触发源：BatchID 非空时，批次侧的事件处理器据此把这一个
// 子任务登记进 Batch 聚合。两个聚合因此各自落库、由事件串起来，而不是共享事务。
//
// Confidence / DurationS 以字符串承载而不是 JSON 数字：事件会被序列化落到日志、
// 消息队列与下游存储，中途任何一个消费者用 float64 反序列化就把精度丢了，
// 而事件一旦发出去就没有机会纠正。
type OnTaskCompleted struct {
	domain_event.BaseDomainEvent
	TaskID     string `json:"taskId"`
	UserID     uint64 `json:"userId"`
	BatchID    string `json:"batchId,omitempty"`
	Symbol     string `json:"symbol"`
	Action     string `json:"action"`
	Confidence string `json:"confidence"`
	DurationS  string `json:"durationSeconds"`
}

func NewOnTaskCompleted(taskID string, userID uint64, batchID, symbol, action string, confidence, durationS decimal.Decimal) *OnTaskCompleted {
	return &OnTaskCompleted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		TaskID:          taskID,
		UserID:          userID,
		BatchID:         batchID,
		Symbol:          symbol,
		Action:          action,
		Confidence:      decimalx.FormatRatio(confidence),
		DurationS:       decimalx.FormatPercent(durationS),
	}
}

func (e *OnTaskCompleted) Name() string { return OnTaskCompletedEventName }

func (e *OnTaskCompleted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTaskFailed 在任务以失败收尾时发出。
//
// Retryable 让批次侧能分辨「这次失败还会重试」与「这个子任务到此为止」：
// 只有后者才应该登记进批次的失败计数，否则一次可恢复的重试会提前把批次判死。
type OnTaskFailed struct {
	domain_event.BaseDomainEvent
	TaskID    string `json:"taskId"`
	UserID    uint64 `json:"userId"`
	BatchID   string `json:"batchId,omitempty"`
	Reason    string `json:"reason"`
	Attempts  int    `json:"attempts"`
	Retryable bool   `json:"retryable"`
}

func NewOnTaskFailed(taskID string, userID uint64, batchID, reason string, attempts int, retryable bool) *OnTaskFailed {
	return &OnTaskFailed{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		TaskID:          taskID,
		UserID:          userID,
		BatchID:         batchID,
		Reason:          reason,
		Attempts:        attempts,
		Retryable:       retryable,
	}
}

func (e *OnTaskFailed) Name() string { return OnTaskFailedEventName }

func (e *OnTaskFailed) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnTaskCanceled 在用户主动取消任务时发出。
type OnTaskCanceled struct {
	domain_event.BaseDomainEvent
	TaskID  string `json:"taskId"`
	UserID  uint64 `json:"userId"`
	BatchID string `json:"batchId,omitempty"`
}

func NewOnTaskCanceled(taskID string, userID uint64, batchID string) *OnTaskCanceled {
	return &OnTaskCanceled{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		TaskID:          taskID,
		UserID:          userID,
		BatchID:         batchID,
	}
}

func (e *OnTaskCanceled) Name() string { return OnTaskCanceledEventName }

func (e *OnTaskCanceled) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
