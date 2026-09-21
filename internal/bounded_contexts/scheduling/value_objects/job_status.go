package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// JobStatus 是定时任务的启用状态。
//
// 三态而不是一个 bool：paused 与 disabled 的区别是**谁按下的暂停键**。
//   - paused：可恢复的临时停摆。既可能是管理员手动按下，也可能是连续失败触发的自动熔断。
//   - disabled：人为停用，表示这条任务在可预见的未来都不该再跑。
//     它不会被自动恢复，也不允许改调度——想改就先启用回来。
//
// 把两者合并成 enabled=false 之后，「自动熔断的任务被运维一键全量恢复」这种操作
// 会连带把本该停用的任务一起唤醒，这正是三态要避免的事。
type JobStatus string

const (
	JobStatusEnabled  JobStatus = "enabled"
	JobStatusPaused   JobStatus = "paused"
	JobStatusDisabled JobStatus = "disabled"
)

// NewJobStatus 解析状态。空串合法，语义为「不限状态」，供列表查询使用。
// 非法值不静默降级：一次前端笔误静默变成「查不出任何任务」比报错难排查得多。
func NewJobStatus(s string) (JobStatus, error) {
	switch JobStatus(strings.TrimSpace(s)) {
	case "", JobStatusEnabled, JobStatusPaused, JobStatusDisabled:
		return JobStatus(strings.TrimSpace(s)), nil
	}
	return "", custom_errors.Invalid("非法的定时任务状态: %s", s)
}

func (s JobStatus) Valid() bool {
	switch s {
	case JobStatusEnabled, JobStatusPaused, JobStatusDisabled:
		return true
	}
	return false
}

// Runnable 表示调度器是否应当把这条任务纳入抢占范围。
// 仓储层正是用它把「只跑启用中的任务」写进 ClaimDue 的 WHERE 里。
func (s JobStatus) Runnable() bool { return s == JobStatusEnabled }

func (s JobStatus) IsZero() bool { return s == "" }

func (s JobStatus) String() string { return string(s) }

func (s JobStatus) DisplayName() string {
	switch s {
	case JobStatusEnabled:
		return "已启用"
	case JobStatusPaused:
		return "已暂停"
	case JobStatusDisabled:
		return "已停用"
	}
	return "未知状态"
}
