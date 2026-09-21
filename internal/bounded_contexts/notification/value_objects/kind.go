// Package value_objects 提供站内通知上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不出现 gorm 标签，也不引用 entities：持久化格式的演进属于 repositories/，
// 由 DTO 承载；值对象是实体的构件，反向依赖会让两者绕成一个环。
//
// 本上下文是一个纯消费者——它的写入路径几乎全部由别的上下文的领域事件触发。
// 正因为如此，这里的枚举必须是收敛的：事件处理器里一个拼错的 kind 若被静默放行，
// 用户会收到一条前端认不出、点不动、也筛不出来的通知，而且没有任何报错。
package value_objects

import (
	"strings"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// NotificationKind 是通知的种类。
//
// 它同时是**去重键的前缀**（见 DedupeKey）：同一个来源聚合在不同种类下应当能各产出
// 一条通知——一个分析任务先失败后重试成功，"analysis_failed" 与 "analysis_completed"
// 是两条互不覆盖的通知，靠的正是 kind 进了去重键。
type NotificationKind string

const (
	// KindAnalysisCompleted 分析任务成功收尾。来源聚合是 analysis.Task。
	KindAnalysisCompleted NotificationKind = "analysis_completed"
	// KindAnalysisFailed 分析任务最终失败（不含还会重试的那几次）。来源聚合是 analysis.Task。
	KindAnalysisFailed NotificationKind = "analysis_failed"
	// KindSyncFailed 行情/基础数据同步整体失败。来源聚合是 stock.SyncRun。
	KindSyncFailed NotificationKind = "sync_failed"
	// KindJobPaused 定时任务因连续失败被自动熔断。来源聚合是 scheduling.ScheduledJob。
	KindJobPaused NotificationKind = "job_paused"
	// KindSystem 系统公告等没有来源聚合的通知。
	KindSystem NotificationKind = "system"
)

// NewNotificationKind 解析通知种类。
//
// 空串**不合法**。这与 analysis.NewStatus 的「空串表示不限」不同，因为 kind 不是
// 列表页的筛选条件，它只出现在写入路径上，而写入路径上没有「不限种类」这种东西。
func NewNotificationKind(s string) (NotificationKind, error) {
	k := NotificationKind(strings.TrimSpace(s))
	if k.Valid() {
		return k, nil
	}
	return "", custom_errors.Invalid("非法的通知种类: %s", s)
}

// RehydrateNotificationKind 跳过校验，仅供从数据库重建使用。
//
// 理由与 identity.RehydrateUsername 相同：已经落库的行是既成事实。读路径再跑一遍
// 今天的校验规则，会让一条历史数据（比如某个已经下线的 kind）把整个通知列表接口打挂——
// 而通知列表恰恰是用户每次进首页都会拉的接口。校验属于写入路径。
func RehydrateNotificationKind(s string) NotificationKind { return NotificationKind(s) }

func (k NotificationKind) Valid() bool {
	switch k {
	case KindAnalysisCompleted, KindAnalysisFailed, KindSyncFailed, KindJobPaused, KindSystem:
		return true
	}
	return false
}

func (k NotificationKind) IsZero() bool { return k == "" }

func (k NotificationKind) String() string { return string(k) }

func (k NotificationKind) DisplayName() string {
	switch k {
	case KindAnalysisCompleted:
		return "分析完成"
	case KindAnalysisFailed:
		return "分析失败"
	case KindSyncFailed:
		return "数据同步失败"
	case KindJobPaused:
		return "定时任务已熔断"
	case KindSystem:
		return "系统通知"
	}
	return "未知通知"
}

// DefaultLevel 是该种类通知的默认提示级别。
//
// 级别与种类分开建模而不是直接从种类派生死：同一种类在不同场景下的严重程度可能不同
// （一次可重试的同步失败与一次彻底放弃的同步失败）。但绝大多数调用点不需要这份区分，
// 给它们一个有依据的默认值，好过让每个事件处理器各自拍脑袋。
func (k NotificationKind) DefaultLevel() NotificationLevel {
	switch k {
	case KindAnalysisCompleted, KindSystem:
		return LevelInfo
	case KindAnalysisFailed, KindJobPaused:
		return LevelWarning
	case KindSyncFailed:
		return LevelError
	}
	return LevelInfo
}
