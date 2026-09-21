package entities

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SyncRun 是本上下文的第二个聚合根（与 Stock 并列）。
//
// # 为什么不是 Stock 的子实体
//
// 一次同步横跨成千上万个 Stock 聚合，它没法住在其中任何一个里面——
// 真要塞进去，就得先决定「这次运行属于哪只股票」，而答案是「都不属于」。
// 它有自己的生命周期（运行中 → 终态 → 按保留期清理），也被运维面板独立查询。
//
// 它只按标识引用别的聚合：Market 是值对象，游标里存的是代码字符串，
// 任何时候都不持有 Stock 实体。同步过程写 Stock 聚合，但那是通过各自的仓储分别落库，
// 绝不会出现一个事务同时跨 SyncRun 和 Stock。
type SyncRun struct {
	domain_event.EventRecorder

	ID     string
	Kind   value_objects.SyncKind
	Market shared_vo.Market
	Status value_objects.SyncStatus
	Stats  value_objects.SyncStats

	// Cursor 是断点续传位置（最后一个处理完的标的代码）。
	// 全量同步动辄几十分钟，进程被杀之后从头再来既慢又会把外部配额白白烧掉一遍。
	Cursor string

	TriggeredBy string // 触发来源：scheduler / 用户 ID / manual
	StartedAt   time.Time
	FinishedAt  *time.Time
	DurationMS  int64
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StartSyncRun 开启一次同步运行。
func StartSyncRun(id string, kind value_objects.SyncKind, market shared_vo.Market, triggeredBy string) (*SyncRun, error) {
	if id == "" {
		return nil, custom_errors.Invalid("同步运行 ID 不能为空")
	}
	if !kind.Valid() {
		return nil, custom_errors.Invalid("非法的同步类型: %s", kind)
	}
	if !market.Valid() {
		return nil, custom_errors.Invalid("非法的市场: %s", market)
	}
	now := time.Now()
	return &SyncRun{
		ID:          id,
		Kind:        kind,
		Market:      market,
		Status:      value_objects.SyncRunning,
		Stats:       value_objects.NewSyncStats(0, 0, 0, 0),
		TriggeredBy: triggeredBy,
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// PlanTotal 在扇出开始前登记总量，好让进度条从一开始就有分母。
func (r *SyncRun) PlanTotal(total int) error {
	if r.Status.Terminal() {
		return custom_errors.Conflict("同步已处于终态 %s，无法调整总量", r.Status)
	}
	r.Stats = r.Stats.WithTotal(total)
	r.UpdatedAt = time.Now()
	return nil
}

// Advance 推进断点并累加分片统计。分片提交是断点续传能成立的前提：
// 只在最后写一次统计，崩溃后就完全不知道处理到哪了。
func (r *SyncRun) Advance(cursor string, succeeded, failed, skipped int) error {
	if r.Status.Terminal() {
		return custom_errors.Conflict("同步已处于终态 %s，无法推进", r.Status)
	}
	if cursor != "" {
		r.Cursor = cursor
	}
	r.Stats = r.Stats.Plus(succeeded, failed, skipped)
	r.UpdatedAt = time.Now()
	return nil
}

// Finish 依据统计结果判定终态。
//
// 「多少算成功、多少算部分成功」是业务规则，因此判定住在聚合里而不是服务里：
// 放在服务里意味着每个调用方都得自己记得这套判定，迟早会出现两套不一致的口径。
func (r *SyncRun) Finish(stats value_objects.SyncStats) error {
	if r.Status.Terminal() {
		return custom_errors.Conflict("同步已处于终态 %s，无法重复结束", r.Status)
	}
	now := time.Now()
	r.Stats = stats
	r.FinishedAt = &now
	r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
	r.UpdatedAt = now

	switch {
	case stats.AllFailed():
		// 一条都没成功，等同于失败——即使每个失败都被单独容忍了。
		r.Status = value_objects.SyncFailed
		r.Error = "全部标的同步失败"
		r.AddDomainEvent(domain_events.NewOnSyncFailed(r.ID, r.Kind.String(), r.Market.String(), r.Error))
	case stats.HasFailure():
		r.Status = value_objects.SyncPartial
		r.AddDomainEvent(domain_events.NewOnSyncCompleted(
			r.ID, r.Kind.String(), r.Market.String(), r.Status.String(),
			stats.Total, stats.Succeeded, stats.Failed, stats.SuccessRate))
	default:
		r.Status = value_objects.SyncSucceeded
		r.AddDomainEvent(domain_events.NewOnSyncCompleted(
			r.ID, r.Kind.String(), r.Market.String(), r.Status.String(),
			stats.Total, stats.Succeeded, stats.Failed, stats.SuccessRate))
	}
	return nil
}

// Fail 整体失败（数据源不可用、参数非法等尚未进入扇出就挂掉的情况）。
func (r *SyncRun) Fail(reason string) error {
	if r.Status.Terminal() {
		return custom_errors.Conflict("同步已处于终态 %s，无法标记失败", r.Status)
	}
	now := time.Now()
	r.Status = value_objects.SyncFailed
	r.Error = reason
	r.FinishedAt = &now
	r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
	r.UpdatedAt = now
	r.AddDomainEvent(domain_events.NewOnSyncFailed(r.ID, r.Kind.String(), r.Market.String(), reason))
	return nil
}

// Cancel 取消运行中的同步（进程优雅退出、运维手动中止）。
func (r *SyncRun) Cancel() error {
	if r.Status.Terminal() {
		return custom_errors.Conflict("同步已处于终态 %s，无法取消", r.Status)
	}
	now := time.Now()
	r.Status = value_objects.SyncCanceled
	r.FinishedAt = &now
	r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
	r.UpdatedAt = now
	return nil
}

// Resumable 判断这次运行是否留下了可续传的断点。
// 只有非正常结束（失败/取消）且已经处理过一部分，续传才有意义。
func (r *SyncRun) Resumable() bool {
	if r.Cursor == "" {
		return false
	}
	return r.Status == value_objects.SyncFailed ||
		r.Status == value_objects.SyncCanceled ||
		r.Status == value_objects.SyncPartial
}

func (r *SyncRun) IsRunning() bool { return r.Status == value_objects.SyncRunning }

// RunningKey 是「同类同市场只允许一个运行中实例」这条不变式在数据库里的载体。
// 运行中返回 "kind:market"，终态返回空——仓储据此写 NULL，
// 让唯一索引只对运行中的行生效（MySQL 没有部分索引，只能靠 NULL 不参与唯一约束这个特性）。
func (r *SyncRun) RunningKey() string {
	if r.Status == value_objects.SyncRunning {
		return r.Kind.String() + ":" + r.Market.String()
	}
	return ""
}
