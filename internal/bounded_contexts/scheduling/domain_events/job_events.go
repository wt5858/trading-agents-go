// Package domain_events 是定时任务上下文对外广播的事实。
//
// 每个事件都对应一次**已经发生且已经确定**的领域决策。消费者（告警、审计、看板）
// 订阅它们，而不是去轮询 scheduled_jobs 表——后者会让调度器的热表被读放大。
package domain_events

import (
	"encoding/json"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

const (
	OnJobExecutedEventName   = "scheduling.job_executed"
	OnJobFailedEventName     = "scheduling.job_failed"
	OnJobAutoPausedEventName = "scheduling.job_auto_paused"
)

// OnJobExecuted 表示一次执行成功收尾。
//
// ItemCount 与 DurationSeconds 一起带出来，是为了让看板不必回查 job_executions：
// 「昨天同步了多少条行情、花了多久」是这个事件本身就该回答的问题。
type OnJobExecuted struct {
	domain_event.BaseDomainEvent
	JobID           string `json:"jobId"`
	JobName         string `json:"jobName"`
	Kind            string `json:"kind"`
	ExecutionID     string `json:"executionId"`
	Summary         string `json:"summary"`
	ItemCount       int    `json:"itemCount"`
	DurationSeconds string `json:"durationSeconds"`
	Manual          bool   `json:"manual"`
}

func NewOnJobExecuted(
	jobID, jobName, kind, executionID, summary string,
	itemCount int, durationSeconds decimal.Decimal, manual bool,
) *OnJobExecuted {
	return &OnJobExecuted{
		BaseDomainEvent: domain_event.NewBaseDomainEvent(),
		JobID:           jobID,
		JobName:         jobName,
		Kind:            kind,
		ExecutionID:     executionID,
		Summary:         summary,
		ItemCount:       itemCount,
		DurationSeconds: durationSeconds.StringFixed(3),
		Manual:          manual,
	}
}

func (e *OnJobExecuted) Name() string { return OnJobExecutedEventName }

func (e *OnJobExecuted) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnJobFailed 表示一次执行失败。
//
// ConsecutiveFailures 与 MaxConsecutiveFailures 一起带出来，消费者据此就能算出
// 「再失败几次就会被自动熔断」，从而在真正熔断之前先告警——
// 等到 OnJobAutoPaused 才通知运维，任务已经停了。
type OnJobFailed struct {
	domain_event.BaseDomainEvent
	JobID                  string `json:"jobId"`
	JobName                string `json:"jobName"`
	Kind                   string `json:"kind"`
	ExecutionID            string `json:"executionId"`
	Reason                 string `json:"reason"`
	ConsecutiveFailures    int    `json:"consecutiveFailures"`
	MaxConsecutiveFailures int    `json:"maxConsecutiveFailures"`
	Manual                 bool   `json:"manual"`
}

func NewOnJobFailed(
	jobID, jobName, kind, executionID, reason string,
	consecutive, max int, manual bool,
) *OnJobFailed {
	return &OnJobFailed{
		BaseDomainEvent:        domain_event.NewBaseDomainEvent(),
		JobID:                  jobID,
		JobName:                jobName,
		Kind:                   kind,
		ExecutionID:            executionID,
		Reason:                 reason,
		ConsecutiveFailures:    consecutive,
		MaxConsecutiveFailures: max,
		Manual:                 manual,
	}
}

func (e *OnJobFailed) Name() string { return OnJobFailedEventName }

func (e *OnJobFailed) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// OnJobAutoPaused 表示任务因连续失败被自动熔断。
//
// 这是本上下文最需要被人看见的事件：从这一刻起任务不再执行，
// 而「任务没有执行」本身是安静的——没有失败日志、没有告警、没有任何动静。
// 如果这个事件没人订阅，一条被熔断的行情同步任务可以静悄悄躺上几个月。
type OnJobAutoPaused struct {
	domain_event.BaseDomainEvent
	JobID               string `json:"jobId"`
	JobName             string `json:"jobName"`
	Kind                string `json:"kind"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	LastReason          string `json:"lastReason"`
}

func NewOnJobAutoPaused(jobID, jobName, kind string, consecutive int, lastReason string) *OnJobAutoPaused {
	return &OnJobAutoPaused{
		BaseDomainEvent:     domain_event.NewBaseDomainEvent(),
		JobID:               jobID,
		JobName:             jobName,
		Kind:                kind,
		ConsecutiveFailures: consecutive,
		LastReason:          lastReason,
	}
}

func (e *OnJobAutoPaused) Name() string { return OnJobAutoPausedEventName }

func (e *OnJobAutoPaused) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}
