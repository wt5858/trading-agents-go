// Package value_objects 提供分析上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不感知数据库与网络。任何需要「身份 + 状态机」的概念（Task / Batch）
// 都不属于这里——它们是 entities/ 的职责。
package value_objects

import "github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"

// Status 是分析任务状态值对象。
//
// 它只描述「状态是什么」与「状态之间的先后关系」，不描述「谁能改状态」——
// 后者是 Task 聚合的状态机职责，放在这里会让任何拿到 Status 的代码都能绕过聚合。
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// NewStatus 解析状态字符串。空串合法，语义为「不限状态」，列表查询会用到。
// 非法值不静默降级而是报错：静默降级会让一次前端笔误变成「查不出任何任务」的哑故障。
func NewStatus(s string) (Status, error) {
	switch Status(s) {
	case "", StatusQueued, StatusRunning, StatusCompleted, StatusFailed, StatusCanceled:
		return Status(s), nil
	}
	return "", custom_errors.Invalid("非法的任务状态: %s", s)
}

func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusRunning, StatusCompleted, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// Terminal 判断是否终态。终态任务不接受任何状态迁移。
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCanceled
}

// Final 判断是否「用户可见的最终结局」。
//
// 它比 Terminal 更窄：failed 是终态，但它只是一次未竟的尝试，仍允许 Requeue 重跑；
// completed / canceled 则是用户已经看到的结论，任何改写都等于让结果凭空变化。
// 仓储层正是用这条谓词把「终局不可改写」写进 UPDATE 的 WHERE 里。
func (s Status) Final() bool {
	return s == StatusCompleted || s == StatusCanceled
}

func (s Status) IsZero() bool { return s == "" }

func (s Status) String() string { return string(s) }

func (s Status) DisplayName() string {
	switch s {
	case StatusQueued:
		return "排队中"
	case StatusRunning:
		return "分析中"
	case StatusCompleted:
		return "已完成"
	case StatusFailed:
		return "已失败"
	case StatusCanceled:
		return "已取消"
	}
	return "未知状态"
}

// FinalStatuses 供仓储层拼 WHERE 谓词使用，避免把状态字面量散落到 SQL 里。
func FinalStatuses() []string {
	return []string{StatusCompleted.String(), StatusCanceled.String()}
}
