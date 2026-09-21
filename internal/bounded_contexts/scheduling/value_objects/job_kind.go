package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// JobKind 是定时任务的种类。
//
// 它同时是**运行器的路由键**：调度器按 Kind 找到注册进来的 JobRunner。
// 因此这里必须是收敛的枚举而不是自由字符串——一个拼错的 kind 若被放行，
// 任务会安静地永远找不到运行器，而不是在创建那一刻就被拒绝。
type JobKind string

const (
	// JobKindMarketSync 同步行情/资讯等外部数据。
	JobKindMarketSync JobKind = "market_sync"
	// JobKindScheduledAnalysis 按计划发起分析任务。
	JobKindScheduledAnalysis JobKind = "scheduled_analysis"
	// JobKindDataCleanup 清理过期数据。
	JobKindDataCleanup JobKind = "data_cleanup"
)

// NewJobKind 解析任务种类。
//
// 空串合法，语义是「不限种类」，只用于列表查询的筛选条件。
// 写路径必须自己用 Valid() 再挡一道——「不限」不是一个可以落库的值。
func NewJobKind(s string) (JobKind, error) {
	switch JobKind(strings.TrimSpace(s)) {
	case "", JobKindMarketSync, JobKindScheduledAnalysis, JobKindDataCleanup:
		return JobKind(strings.TrimSpace(s)), nil
	}
	return "", custom_errors.Invalid("非法的定时任务种类: %s", s)
}

func (k JobKind) Valid() bool {
	switch k {
	case JobKindMarketSync, JobKindScheduledAnalysis, JobKindDataCleanup:
		return true
	}
	return false
}

func (k JobKind) IsZero() bool { return k == "" }

func (k JobKind) String() string { return string(k) }

func (k JobKind) DisplayName() string {
	switch k {
	case JobKindMarketSync:
		return "行情同步"
	case JobKindScheduledAnalysis:
		return "定时分析"
	case JobKindDataCleanup:
		return "数据清理"
	}
	return "未知种类"
}
