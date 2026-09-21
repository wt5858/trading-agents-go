package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ExecutionStatus 是一次执行所处的阶段。
//
// # 生命周期
//
//	queued -> running -> succeeded / failed / skipped
//
// queued 是调度器抢到一次触发后立刻写下的状态，此时消息才刚发往队列。
// 它同时扮演两个角色：一条「这次触发欠着没跑」的欠条，和消费端抢占的对象——
// 「把 queued 改成 running」这一条 UPDATE 命中几行，就是「谁拿到了这次执行」的答案。
// 没有它，消息投递失败就意味着一次触发凭空消失，而且不留任何痕迹。
//
// # 为什么 skipped 与 failed 分开
//
// 跳过的原因是**部署侧的配置问题**（这个 kind 的运行器没有注册到本进程），
// 任务本身的目标并没有坏。把它算成失败会让连续失败计数往上走，
// 最终把一条完全正常的任务自动熔断掉——一次发布漏配就能让所有定时任务集体暂停。
type ExecutionStatus string

const (
	ExecutionStatusQueued    ExecutionStatus = "queued"
	ExecutionStatusRunning   ExecutionStatus = "running"
	ExecutionStatusSucceeded ExecutionStatus = "succeeded"
	ExecutionStatusFailed    ExecutionStatus = "failed"
	ExecutionStatusSkipped   ExecutionStatus = "skipped"
)

// NewExecutionStatus 解析执行状态。空串合法，语义为「不限状态」。
func NewExecutionStatus(s string) (ExecutionStatus, error) {
	switch ExecutionStatus(strings.TrimSpace(s)) {
	case "", ExecutionStatusQueued, ExecutionStatusRunning,
		ExecutionStatusSucceeded, ExecutionStatusFailed, ExecutionStatusSkipped:
		return ExecutionStatus(strings.TrimSpace(s)), nil
	}
	return "", custom_errors.Invalid("非法的执行状态: %s", s)
}

func (s ExecutionStatus) Valid() bool {
	switch s {
	case ExecutionStatusQueued, ExecutionStatusRunning,
		ExecutionStatusSucceeded, ExecutionStatusFailed, ExecutionStatusSkipped:
		return true
	}
	return false
}

// Terminal 判断是否终态。终态的执行记录不再接受任何改写——
// 它已经是一条审计记录，改写它等于篡改历史。
func (s ExecutionStatus) Terminal() bool {
	return s == ExecutionStatusSucceeded || s == ExecutionStatusFailed || s == ExecutionStatusSkipped
}

// Pending 判断这次执行是否还欠着。
//
// 恢复巡检要找的就是这两种状态卡太久的记录：queued 卡住说明消息没送到，
// running 卡住说明消费者在执行途中死了。两者的处置不同，但「需要被看一眼」是一样的。
func (s ExecutionStatus) Pending() bool {
	return s == ExecutionStatusQueued || s == ExecutionStatusRunning
}

func (s ExecutionStatus) IsZero() bool { return s == "" }

func (s ExecutionStatus) String() string { return string(s) }

func (s ExecutionStatus) DisplayName() string {
	switch s {
	case ExecutionStatusQueued:
		return "待执行"
	case ExecutionStatusRunning:
		return "执行中"
	case ExecutionStatusSucceeded:
		return "成功"
	case ExecutionStatusFailed:
		return "失败"
	case ExecutionStatusSkipped:
		return "已跳过"
	}
	return "未知状态"
}
