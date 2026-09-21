package domain_events

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnBatchSubmittedEventName = "analysis.batch_submitted"
	OnBatchDegradedEventName  = "analysis.batch_degraded"
	OnBatchCompletedEventName = "analysis.batch_completed"
)

// OnBatchSubmitted 在批次落库成功、子任务尚未写入时发出。
//
// 它是整条批量提交链路的「意图声明」：批次行 + 这条事件一起说明
// 「系统承诺要跑这 N 个 ID」。即使后续的子任务写入半途失败，
// 对账程序也能凭它把差额补成失败，批次不会永远停在 99%。
type OnBatchSubmitted struct {
	domain_event.BaseDomainEvent
	BatchID string   `json:"batchId"`
	UserID  uint64   `json:"userId"`
	TaskIDs []string `json:"taskIds"`
	Total   int      `json:"total"`
}

func NewOnBatchSubmitted(batchID string, userID uint64, taskIDs []string) *OnBatchSubmitted {
	ids := append([]string(nil), taskIDs...)
	return &OnBatchSubmitted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		BatchID:         batchID,
		UserID:          userID,
		TaskIDs:         ids,
		Total:           len(ids),
	}
}

func (e *OnBatchSubmitted) Name() string { return OnBatchSubmittedEventName }

func (e *OnBatchSubmitted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnBatchDegraded 在批次里有子任务未能落库（或未能入队）、被直接判失败时发出。
//
// 它存在的意义是「让不一致可见」：批次仍然是自洽的（总数不变、差额记成失败），
// 但运维需要知道这一批不是完整跑完的。
type OnBatchDegraded struct {
	domain_event.BaseDomainEvent
	BatchID     string   `json:"batchId"`
	UserID      uint64   `json:"userId"`
	DroppedIDs  []string `json:"droppedTaskIds"`
	Reason      string   `json:"reason"`
	DroppedSize int      `json:"droppedCount"`
}

func NewOnBatchDegraded(batchID string, userID uint64, droppedIDs []string, reason string) *OnBatchDegraded {
	ids := append([]string(nil), droppedIDs...)
	return &OnBatchDegraded{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		BatchID:         batchID,
		UserID:          userID,
		DroppedIDs:      ids,
		Reason:          reason,
		DroppedSize:     len(ids),
	}
}

func (e *OnBatchDegraded) Name() string { return OnBatchDegradedEventName }

func (e *OnBatchDegraded) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnBatchCompleted 在批次的全部子任务都已结算时发出，供报告上下文汇总批量结论。
type OnBatchCompleted struct {
	domain_event.BaseDomainEvent
	BatchID   string `json:"batchId"`
	UserID    uint64 `json:"userId"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
}

func NewOnBatchCompleted(batchID string, userID uint64, total, completed, failed int) *OnBatchCompleted {
	return &OnBatchCompleted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		BatchID:         batchID,
		UserID:          userID,
		Total:           total,
		Completed:       completed,
		Failed:          failed,
	}
}

func (e *OnBatchCompleted) Name() string { return OnBatchCompletedEventName }

func (e *OnBatchCompleted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
