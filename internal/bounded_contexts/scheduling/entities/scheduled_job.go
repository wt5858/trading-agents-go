// Package entities 承载定时任务上下文的全部业务不变式。
//
// 本包不感知 HTTP、数据库与事务——那些分别属于 application/ 与 repositories/。
//
// 本上下文有两个聚合根：ScheduledJob 与 JobExecution。它们只通过 ID 互相引用，
// 各自独立落库，没有任何一个事务会同时写这两张表。理由见 job_execution.go 顶部。
package entities

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

const (
	// DefaultMaxConsecutiveFailures 是自动熔断的默认阈值。
	//
	// 5 次是一个刻意的折中：外部数据源的抖动通常一两次就恢复，
	// 阈值太低会把正常抖动误判成永久故障；太高则意味着一个彻底坏掉的目标
	// （接口下线、凭据过期）会被重试整整一天才停下来。
	DefaultMaxConsecutiveFailures = 5

	// DefaultTimeout 是单次执行的默认超时。
	DefaultTimeout = 10 * time.Minute

	// MaxTimeout 是超时上限，也是业务不变式而非配置。
	//
	// 单次执行会独占一个并发名额。允许把超时设成「一天」，等于允许一条卡死的任务
	// 永久吃掉调度器的一格并发；几条这样的任务就能让整个 sweep 停摆。
	MaxTimeout = 2 * time.Hour

	// maxJobNameLen 与仓储层 name 列的宽度对齐。
	maxJobNameLen = 128
)

// ScheduledJob 是定时任务的聚合根：有身份（ID）、有状态机（enabled/paused/disabled）、
// 有一条必须被守住的核心不变式（连续失败到阈值必须自动熔断）。
//
// # 字段全部导出
//
// 与团队既有服务保持一致：不变式靠「唯一的写入方法」来守，而不是靠私有字段 + 一堆 getter。
// 任何状态迁移都必须经由下面的意图方法，这样每条规则只有一个执行点。
//
// # 派生量随聚合落库
//
// SuccessRate 是 SuccessRuns ÷ TotalRuns × 100，在写路径算一次并持久化。
// 读路径直接取这个字段，绝不重算：重算会让列表接口和详情接口在并发写入时
// 给出互相矛盾的数字，也让「历史成功率」这个概念随每次查询漂移。
type ScheduledJob struct {
	domain_event.EventRecorder

	ID      string
	Name    string
	Kind    value_objects.JobKind
	Cron    value_objects.CronExpression
	Payload value_objects.JobPayload
	Status  value_objects.JobStatus

	// NextRunAt 是下一次计划触发时刻，也是分布式抢占的**乐观锁版本号**。
	// 仓储层的 ClaimDue 用 `WHERE next_run_at = ?` 把它当成 CAS 的期望值：
	// 谁把它推进成功，谁就独占了这一次触发。详见 repositories/scheduled_job_repository.go。
	NextRunAt  time.Time
	LastRunAt  *time.Time
	LastStatus value_objects.ExecutionStatus

	// ConsecutiveFailures 是自动熔断的计数器：成功清零，失败自增。
	// 它必须是「连续」失败数而不是累计失败数——一条跑了一年、偶尔失败几次的任务
	// 用累计数早就被熔断了，而它其实一直是健康的。
	ConsecutiveFailures    int
	MaxConsecutiveFailures int

	Timeout time.Duration

	// 下面三个是执行统计。TotalRuns / SuccessRuns 是事实计数，
	// SuccessRate 是它们派生出的固化值（见类型注释）。
	TotalRuns   int64
	SuccessRuns int64
	SuccessRate decimal.Decimal

	CreatedBy uint64
	CreatedAt time.Time
	UpdatedAt time.Time

	// ClaimedFor 是本次抢到的那一次触发的计划时刻，**不落库**。
	//
	// 它是一次抢占在内存里的凭据：ClaimOccurrence 把 NextRunAt 推进之后，
	// 原来的值仍然是这次执行的「计划触发时刻」，执行记录要用它。
	// 它的持久化形态是 job_executions.scheduled_for——同一个事实由审计聚合负责保存，
	// 在 scheduled_jobs 上再存一列只会制造两份可能分叉的真相。
	ClaimedFor time.Time
}

// Schedule 创建一条定时任务。这是任务进入系统的唯一入口。
//
// 构造时就把首次 NextRunAt 算出来：任务一旦落库就应当是「可被调度器捞到」的完整状态，
// 留一个零值等着某个后置流程去补，等于给系统留了一个永远不会执行的任务的可能。
func Schedule(
	id, name string,
	kind value_objects.JobKind,
	spec value_objects.CronExpression,
	payload value_objects.JobPayload,
	maxConsecutiveFailures int,
	timeout time.Duration,
	createdBy uint64,
) (*ScheduledJob, error) {
	if id == "" {
		return nil, custom_errors.Invalid("任务 ID 不能为空")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, custom_errors.Invalid("任务名称不能为空")
	}
	if len(name) > maxJobNameLen {
		return nil, custom_errors.Invalid("任务名称不能超过 %d 个字符", maxJobNameLen)
	}
	if !kind.Valid() {
		return nil, custom_errors.Invalid("任务种类不合法: %s", kind.String())
	}
	if !spec.Valid() {
		// 走到这里说明调用方绕过了 NewCronExpression 或者传了 Rehydrate 出来的坏值。
		// 一条算不出下次执行时间的任务落库之后是**静默失效**的，必须在入口拦住。
		return nil, custom_errors.Invalid("cron 表达式不可用，无法计算下次执行时间")
	}
	if createdBy == 0 {
		return nil, custom_errors.Invalid("任务必须记录创建人")
	}
	if maxConsecutiveFailures <= 0 {
		maxConsecutiveFailures = DefaultMaxConsecutiveFailures
	}
	timeout, err := normalizeTimeout(timeout)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	next := spec.Next(now)
	if next.IsZero() {
		return nil, custom_errors.Invalid("cron 表达式 %q 在未来不会再触发", spec.String())
	}

	return &ScheduledJob{
		ID:                     id,
		Name:                   name,
		Kind:                   kind,
		Cron:                   spec,
		Payload:                payload,
		Status:                 value_objects.JobStatusEnabled,
		NextRunAt:              next,
		MaxConsecutiveFailures: maxConsecutiveFailures,
		Timeout:                timeout,
		CreatedBy:              createdBy,
		CreatedAt:              now,
		UpdatedAt:              now,
	}, nil
}

// DueAt 判断任务在 now 这一刻是否到期。
//
// 只有启用中的任务才算到期：暂停/停用的任务 NextRunAt 仍然停在过去，
// 不把状态一并判掉的话，一条暂停三天的任务在恢复的瞬间会被判定为「早就该跑了」。
func (j *ScheduledJob) DueAt(now time.Time) bool {
	if !j.Status.Runnable() {
		return false
	}
	if j.NextRunAt.IsZero() {
		return false
	}
	return !now.Before(j.NextRunAt)
}

// ClaimOccurrence 抢占「这一次」触发，把 NextRunAt 推进到下一个触发时刻。
//
// 返回值是本次抢到的那一次触发的**计划时刻**（推进前的 NextRunAt），
// 它和实际执行时刻不同——调度器每 30 秒扫一次，实际执行总是晚一点。
// 执行记录要存计划时刻，否则「这个任务有没有按点跑」就再也说不清了。
//
// # 为什么从 now 往后算，而不是从 scheduledFor 往后算
//
// 一台 worker 停机一天后重启，某条每分钟执行的任务积压了 1440 次触发。
// 若按 scheduledFor 逐次推进，调度器会连续补跑 1440 次——把一次停机放大成一场雪崩。
// 从 now 往后算等于**丢弃错过的触发**，只跑最近的一次。
// 定时任务的语义是「到点做一次」而不是「一次都不能少」；真需要补数据的场景
// 应该由任务自己的参数表达（例如同步任务自带起止区间），而不是靠调度器堆次数。
//
// # 为什么这里还要再判一次状态
//
// 这是分布式抢占的**内存半场**。仓储层的条件 UPDATE 是另一半，两者缺一不可：
// 单靠这里守不住多副本（两个副本各自在内存里判定成功），
// 单靠仓储层则会让一条被暂停的任务也算出新的 NextRunAt。
func (j *ScheduledJob) ClaimOccurrence(now time.Time) (time.Time, error) {
	if !j.Status.Runnable() {
		return time.Time{}, custom_errors.Conflict("任务处于%s状态，不可执行", j.Status.DisplayName())
	}
	if !j.DueAt(now) {
		return time.Time{}, custom_errors.Conflict("任务尚未到期，下次执行时间 %s",
			j.NextRunAt.Format(time.RFC3339))
	}
	next := j.Cron.Next(now)
	if next.IsZero() {
		return time.Time{}, custom_errors.Invalid("cron 表达式 %q 不可用，无法推进下次执行时间", j.Cron.String())
	}

	scheduledFor := j.NextRunAt
	j.NextRunAt = next
	j.ClaimedFor = scheduledFor
	j.UpdatedAt = now
	return scheduledFor, nil
}

// RecordSuccess 登记一次成功执行。
//
// 清零 ConsecutiveFailures 是这里最重要的一行：熔断计数器的语义是「连续」，
// 一次成功就说明目标已经恢复，之前的失败不该继续累积。
func (j *ScheduledJob) RecordSuccess(executionID string, at time.Time, summary string, itemCount int, duration time.Duration, manual bool) {
	j.ConsecutiveFailures = 0
	runAt := at
	j.LastRunAt = &runAt
	j.LastStatus = value_objects.ExecutionStatusSucceeded
	j.TotalRuns++
	j.SuccessRuns++
	j.refreshSuccessRate()
	j.UpdatedAt = at

	j.AddDomainEvent(domain_events.NewOnJobExecuted(
		j.ID, j.Name, j.Kind.String(), executionID, summary, itemCount, decimal.NewFromInt(duration.Milliseconds()).DivRound(decimal.NewFromInt(1000), 3), manual))
}

// RecordFailure 登记一次失败，并在触及阈值时自动熔断。
//
// # 这是本聚合的核心不变式
//
// 一条目标已经永久损坏的任务（接口下线、凭据过期、表被删了）如果没有熔断，
// 会以它的 cron 频率永远重试下去：每分钟一次的任务一天就是 1440 次无效调用，
// 打爆日志、打爆上游的限流、打爆告警通道，而且这种噪声会持续到有人手动介入。
// 「失败 N 次就停下来」必须是聚合的规则，不能是调度器的一段 if——
// 手动触发路径、sweep 路径、将来的任何路径都必须同样受它约束。
//
// 熔断用 paused 而不是 disabled：它是可恢复的。Resume 会把计数器清零，
// 让运维修好目标之后一键恢复，而不是重建任务。
func (j *ScheduledJob) RecordFailure(executionID string, at time.Time, reason string, manual bool) {
	if strings.TrimSpace(reason) == "" {
		reason = "任务执行失败"
	}
	j.ConsecutiveFailures++
	runAt := at
	j.LastRunAt = &runAt
	j.LastStatus = value_objects.ExecutionStatusFailed
	j.TotalRuns++
	j.refreshSuccessRate()
	j.UpdatedAt = at

	j.AddDomainEvent(domain_events.NewOnJobFailed(
		j.ID, j.Name, j.Kind.String(), executionID, reason,
		j.ConsecutiveFailures, j.MaxConsecutiveFailures, manual))

	// 只熔断启用中的任务：已经被暂停的任务不需要再熔断一次，
	// 否则手动触发一条暂停中的任务会重复发出 OnJobAutoPaused。
	if j.MaxConsecutiveFailures > 0 &&
		j.ConsecutiveFailures >= j.MaxConsecutiveFailures &&
		j.Status == value_objects.JobStatusEnabled {
		j.Status = value_objects.JobStatusPaused
		j.AddDomainEvent(domain_events.NewOnJobAutoPaused(
			j.ID, j.Name, j.Kind.String(), j.ConsecutiveFailures, reason))
	}
}

// RecordSkipped 登记一次跳过。
//
// 不动 ConsecutiveFailures：跳过的原因是本进程没注册对应的运行器，
// 属于部署问题而不是任务问题。把它算成失败，一次发布漏配就能在几轮 sweep 之后
// 把所有该 kind 的任务集体熔断掉——故障范围被平白放大了一个数量级。
// 也不计入 TotalRuns：它压根没执行，计进去会污染成功率。
func (j *ScheduledJob) RecordSkipped(at time.Time) {
	j.LastStatus = value_objects.ExecutionStatusSkipped
	runAt := at
	j.LastRunAt = &runAt
	j.UpdatedAt = at
}

// Pause 手动暂停。
func (j *ScheduledJob) Pause() error {
	switch j.Status {
	case value_objects.JobStatusPaused:
		return custom_errors.Conflict("任务已处于暂停状态")
	case value_objects.JobStatusDisabled:
		return custom_errors.Conflict("任务已停用，无需暂停")
	}
	j.Status = value_objects.JobStatusPaused
	j.UpdatedAt = time.Now()
	return nil
}

// Resume 恢复执行。
//
// 做两件在别处很容易漏掉的事：
//
//  1. 清零 ConsecutiveFailures。恢复一条被熔断的任务时，计数器还停在阈值上，
//     不清零的话下一次失败就会立刻再次熔断——运维会以为「恢复按钮没用」。
//  2. 按当前时刻重算 NextRunAt。暂停期间的 NextRunAt 早已成为过去，
//     直接恢复会让任务在恢复的瞬间立刻触发一次，而那次触发没有任何业务意义。
func (j *ScheduledJob) Resume() error {
	switch j.Status {
	case value_objects.JobStatusEnabled:
		return custom_errors.Conflict("任务已处于启用状态")
	case value_objects.JobStatusDisabled:
		return custom_errors.Conflict("任务已停用，需先重新启用才能恢复调度")
	}
	now := time.Now()
	next := j.Cron.Next(now)
	if next.IsZero() {
		return custom_errors.Invalid("cron 表达式 %q 不可用，无法恢复调度", j.Cron.String())
	}
	j.Status = value_objects.JobStatusEnabled
	j.ConsecutiveFailures = 0
	j.NextRunAt = next
	j.UpdatedAt = now
	return nil
}

// Disable 停用任务。与 Pause 的区别见 value_objects.JobStatus 的注释。
func (j *ScheduledJob) Disable() error {
	if j.Status == value_objects.JobStatusDisabled {
		return custom_errors.Conflict("任务已处于停用状态")
	}
	j.Status = value_objects.JobStatusDisabled
	j.UpdatedAt = time.Now()
	return nil
}

// UpdateSchedule 改调度表达式，并立刻按新表达式重算 NextRunAt。
//
// 改了表达式却不重算下次执行时间，是这类系统最经典的一个坑：任务在管理界面上
// 显示成「每小时」，实际却还按旧的「每天」触发，直到下一次触发之后才生效。
// 两件事必须原子地一起做，所以它们在同一个方法里。
func (j *ScheduledJob) UpdateSchedule(spec value_objects.CronExpression) error {
	if j.Status == value_objects.JobStatusDisabled {
		return custom_errors.Conflict("任务已停用，不允许修改调度表达式")
	}
	if !spec.Valid() {
		return custom_errors.Invalid("cron 表达式不可用，无法计算下次执行时间")
	}
	now := time.Now()
	next := spec.Next(now)
	if next.IsZero() {
		return custom_errors.Invalid("cron 表达式 %q 在未来不会再触发", spec.String())
	}
	j.Cron = spec
	j.NextRunAt = next
	j.UpdatedAt = now
	return nil
}

// Reconfigure 修改任务的非调度属性。
//
// 传零值表示「不改这一项」，让 HTTP 层可以做部分更新而不必先读一遍再回填。
// Kind 不在可改之列：它是运行器的路由键，改 kind 等于把任务换成了另一个任务，
// 而历史执行记录仍然挂在同一个 job_id 上，审计链会当场错乱——重建一条更诚实。
func (j *ScheduledJob) Reconfigure(name string, payload *value_objects.JobPayload, timeout time.Duration, maxConsecutiveFailures int) error {
	if j.Status == value_objects.JobStatusDisabled {
		return custom_errors.Conflict("任务已停用，不允许修改配置")
	}
	if name = strings.TrimSpace(name); name != "" {
		if len(name) > maxJobNameLen {
			return custom_errors.Invalid("任务名称不能超过 %d 个字符", maxJobNameLen)
		}
		j.Name = name
	}
	if payload != nil {
		j.Payload = *payload
	}
	if timeout > 0 {
		normalized, err := normalizeTimeout(timeout)
		if err != nil {
			return err
		}
		j.Timeout = normalized
	}
	if maxConsecutiveFailures > 0 {
		j.MaxConsecutiveFailures = maxConsecutiveFailures
	}
	j.UpdatedAt = time.Now()
	return nil
}

// EffectiveTimeout 给出实际生效的超时，兜住历史数据里的零值与越界值。
// 读路径上的兜底而非静默改写：库里的行不因为一次读取而被改。
func (j *ScheduledJob) EffectiveTimeout() time.Duration {
	if j.Timeout <= 0 {
		return DefaultTimeout
	}
	if j.Timeout > MaxTimeout {
		return MaxTimeout
	}
	return j.Timeout
}

// MarkManualOccurrence 为一次手动触发准备执行凭据。
//
// 手动触发**不消耗**计划内的那一次触发：NextRunAt 原封不动。
// 理由很实际——管理员点「立即执行」是为了验证任务能不能跑通，
// 如果这一下把当天的计划触发吃掉了，定时任务当天就不会再自动执行，
// 而没人会预期一次点击有这种副作用。
func (j *ScheduledJob) MarkManualOccurrence(now time.Time) error {
	if j.Status == value_objects.JobStatusDisabled {
		return custom_errors.Conflict("任务已停用，不可手动触发")
	}
	j.ClaimedFor = now
	return nil
}

// refreshSuccessRate 固化派生量。
//
// 百分比在写路径算一次并随聚合落库，读路径直接取。
// 与 analysis.Batch.Percent 同理：读路径重算会让同一条记录每刷新一次给出不同的数字。
func (j *ScheduledJob) refreshSuccessRate() {
	if j.TotalRuns <= 0 {
		j.SuccessRate = decimal.Zero
		return
	}
	j.SuccessRate = decimalx.RoundPercent(
		decimal.NewFromInt(int64(j.SuccessRuns)).
			Div(decimal.NewFromInt(int64(j.TotalRuns))).
			Mul(decimal.NewFromInt(100)),
	)
}

func normalizeTimeout(d time.Duration) (time.Duration, error) {
	if d <= 0 {
		return DefaultTimeout, nil
	}
	if d > MaxTimeout {
		return 0, custom_errors.Invalid("单次执行超时不能超过 %s", MaxTimeout)
	}
	return d, nil
}
