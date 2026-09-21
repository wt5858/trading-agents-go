package entities

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// newTestJob 造一条每分钟执行的任务。每分钟是刻意选的：
// 它让「下一次触发」永远在一分钟之内，测试不必等待也不必造假时钟。
func newTestJob(t *testing.T, maxFailures int) *ScheduledJob {
	t.Helper()
	spec, err := value_objects.NewCronExpression("* * * * *")
	if err != nil {
		t.Fatalf("构造 cron 表达式失败: %v", err)
	}
	job, err := Schedule(
		"job_test_1", "行情同步", value_objects.JobKindMarketSync,
		spec, value_objects.JobPayload{}, maxFailures, time.Minute, 42,
	)
	if err != nil {
		t.Fatalf("创建定时任务失败: %v", err)
	}
	return job
}

// hasEvent 判断本次操作有没有抛出某个事件。注意 GetAllPendingEvents 是「读即清空」，
// 每一步最多只能调一次，因此这里把断言收口成一个helper。
func hasEvent(events []domain_event.DomainEvent, name string) bool {
	for _, e := range events {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 核心不变式：连续失败 N 次必须自动熔断
// ---------------------------------------------------------------------------

// TestRecordFailure_AutoPausesAtThreshold 守住本聚合最重要的一条规则。
//
// 没有这条规则，一条目标已经永久损坏的任务会以它的 cron 频率永远重试下去：
// 每分钟一次就是一天 1440 次无效调用，打爆日志、打爆上游限流、打爆告警。
func TestRecordFailure_AutoPausesAtThreshold(t *testing.T) {
	const threshold = 3
	job := newTestJob(t, threshold)
	now := time.Now()

	// 前 threshold-1 次失败：计数在涨，但任务必须仍然启用。
	// 「阈值前不能提前熔断」和「到阈值必须熔断」是同一条规则的两面，
	// 只测后者会让一个 off-by-one 的实现照样通过。
	for i := 1; i < threshold; i++ {
		job.RecordFailure("exec_x", now, "上游 500", false)
		if job.Status != value_objects.JobStatusEnabled {
			t.Fatalf("第 %d 次失败（阈值 %d）不该熔断，实际状态 %s", i, threshold, job.Status)
		}
		if job.ConsecutiveFailures != i {
			t.Fatalf("连续失败计数应为 %d，实际 %d", i, job.ConsecutiveFailures)
		}
		events := job.GetAllPendingEvents()
		if !hasEvent(events, domain_events.OnJobFailedEventName) {
			t.Fatalf("第 %d 次失败应抛出 %s", i, domain_events.OnJobFailedEventName)
		}
		if hasEvent(events, domain_events.OnJobAutoPausedEventName) {
			t.Fatalf("第 %d 次失败不该抛出熔断事件", i)
		}
	}

	// 第 threshold 次：必须熔断。
	job.RecordFailure("exec_x", now, "上游 500", false)
	if job.Status != value_objects.JobStatusPaused {
		t.Fatalf("连续失败达到阈值 %d 必须自动熔断，实际状态 %s", threshold, job.Status)
	}
	if job.ConsecutiveFailures != threshold {
		t.Fatalf("连续失败计数应为 %d，实际 %d", threshold, job.ConsecutiveFailures)
	}
	events := job.GetAllPendingEvents()
	if !hasEvent(events, domain_events.OnJobAutoPausedEventName) {
		t.Fatalf("熔断必须抛出 %s——它是运维唯一能看见这件事的信号",
			domain_events.OnJobAutoPausedEventName)
	}

	// 熔断之后再失败，不该重复抛出熔断事件，否则告警会被刷屏。
	job.RecordFailure("exec_x", now, "上游 500", false)
	if hasEvent(job.GetAllPendingEvents(), domain_events.OnJobAutoPausedEventName) {
		t.Fatal("已熔断的任务不该重复抛出熔断事件")
	}
}

// TestRecordSuccess_ResetsConsecutiveFailures 守住「连续」这个词。
// 用累计失败数实现的话，一条跑了一年、偶尔失败几次的健康任务早就被熔断了。
func TestRecordSuccess_ResetsConsecutiveFailures(t *testing.T) {
	const threshold = 3
	job := newTestJob(t, threshold)
	now := time.Now()

	job.RecordFailure("e1", now, "抖动", false)
	job.RecordFailure("e2", now, "抖动", false)
	job.GetAllPendingEvents()

	job.RecordSuccess("e3", now, "同步完成", 120, 2*time.Second, false)
	if job.ConsecutiveFailures != 0 {
		t.Fatalf("一次成功必须清零连续失败计数，实际 %d", job.ConsecutiveFailures)
	}
	if !hasEvent(job.GetAllPendingEvents(), domain_events.OnJobExecutedEventName) {
		t.Fatalf("成功执行应抛出 %s", domain_events.OnJobExecutedEventName)
	}

	// 清零之后再失败 threshold-1 次，任务必须仍然启用。
	for i := 0; i < threshold-1; i++ {
		job.RecordFailure("e4", now, "抖动", false)
	}
	if job.Status != value_objects.JobStatusEnabled {
		t.Fatalf("成功清零后的失败计数不该沿用历史，实际状态 %s", job.Status)
	}
}

// TestRecordSkipped_DoesNotCountAsFailure 守住「跳过不是失败」。
//
// 跳过的原因是本进程没注册对应运行器——部署问题而非任务问题。
// 算成失败的话，一次发布漏配就能在几轮 sweep 之后把该种类的任务集体熔断，
// 故障范围被平白放大一个数量级。
func TestRecordSkipped_DoesNotCountAsFailure(t *testing.T) {
	job := newTestJob(t, 2)
	now := time.Now()

	job.RecordSkipped(now)
	job.RecordSkipped(now)
	job.RecordSkipped(now)

	if job.ConsecutiveFailures != 0 {
		t.Fatalf("跳过不该累加失败计数，实际 %d", job.ConsecutiveFailures)
	}
	if job.Status != value_objects.JobStatusEnabled {
		t.Fatalf("连续跳过不该熔断，实际状态 %s", job.Status)
	}
	if job.TotalRuns != 0 {
		t.Fatalf("跳过不该计入执行次数，否则会污染成功率，实际 %d", job.TotalRuns)
	}
}

// TestSuccessRateIsPersistedDerivedValue 守住「派生量写路径固化」这条规则。
func TestSuccessRateIsPersistedDerivedValue(t *testing.T) {
	job := newTestJob(t, 10)
	now := time.Now()

	job.RecordSuccess("e1", now, "ok", 1, time.Second, false)
	job.RecordSuccess("e2", now, "ok", 1, time.Second, false)
	job.RecordFailure("e3", now, "boom", false)
	job.RecordFailure("e4", now, "boom", false)
	job.GetAllPendingEvents()

	if job.TotalRuns != 4 || job.SuccessRuns != 2 {
		t.Fatalf("执行计数不符：total=%d success=%d", job.TotalRuns, job.SuccessRuns)
	}
	if !job.SuccessRate.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("成功率应在写路径算好并固化为 50，实际 %v", job.SuccessRate)
	}
}

// ---------------------------------------------------------------------------
// 抢占的内存半场
// ---------------------------------------------------------------------------

// TestClaimOccurrence_AdvancesAndGuards 覆盖分布式抢占的内存半场。
//
// 仓储层的条件 UPDATE 是另一半（见 repositories/claim_due_test.go）。
// 两者缺一不可：单靠聚合守不住多副本，单靠 SQL 则会让暂停中的任务也算出新的触发时刻。
func TestClaimOccurrence_AdvancesAndGuards(t *testing.T) {
	t.Run("到期时抢占成功并推进下次触发时刻", func(t *testing.T) {
		job := newTestJob(t, 3)
		// 把时钟拨到计划触发时刻之后，模拟「这一次已经到期」。
		now := job.NextRunAt.Add(time.Second)
		before := job.NextRunAt

		scheduledFor, err := job.ClaimOccurrence(now)
		if err != nil {
			t.Fatalf("到期任务应当可以被抢占: %v", err)
		}
		// 返回的必须是**计划**触发时刻，不是当前时刻：
		// 只有它能回答「这个任务有没有按点跑」。
		if !scheduledFor.Equal(before) {
			t.Fatalf("应返回推进前的计划触发时刻 %s，实际 %s", before, scheduledFor)
		}
		if !job.NextRunAt.After(now) {
			t.Fatalf("抢占后 NextRunAt 必须推进到 now 之后，实际 %s", job.NextRunAt)
		}
		if !job.ClaimedFor.Equal(before) {
			t.Fatalf("ClaimedFor 应记录本次抢到的计划时刻 %s，实际 %s", before, job.ClaimedFor)
		}
	})

	t.Run("重复抢占同一次触发会被拒绝", func(t *testing.T) {
		job := newTestJob(t, 3)
		now := job.NextRunAt.Add(time.Second)
		if _, err := job.ClaimOccurrence(now); err != nil {
			t.Fatalf("首次抢占应当成功: %v", err)
		}
		// NextRunAt 已经被推进到未来，第二次在同一时刻抢占必须失败。
		// 这正是「同一次触发只会被执行一次」在内存里的表达。
		_, err := job.ClaimOccurrence(now)
		if err == nil {
			t.Fatal("同一次触发不该被抢占两次")
		}
		if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
			t.Fatalf("重复抢占应返回 %s，实际 %s", custom_errors.CodeConflict, code)
		}
	})

	t.Run("未到期的任务不可抢占", func(t *testing.T) {
		job := newTestJob(t, 3)
		// 计划触发时刻之前一秒。
		now := job.NextRunAt.Add(-time.Second)
		if job.DueAt(now) {
			t.Fatal("未到期的任务 DueAt 应为 false")
		}
		if _, err := job.ClaimOccurrence(now); err == nil {
			t.Fatal("未到期的任务不该被抢占")
		}
	})

	t.Run("非启用状态的任务不可抢占", func(t *testing.T) {
		for _, status := range []value_objects.JobStatus{
			value_objects.JobStatusPaused,
			value_objects.JobStatusDisabled,
		} {
			job := newTestJob(t, 3)
			job.Status = status
			now := job.NextRunAt.Add(time.Hour)

			if job.DueAt(now) {
				t.Fatalf("%s 状态的任务不该被判为到期", status)
			}
			_, err := job.ClaimOccurrence(now)
			if err == nil {
				t.Fatalf("%s 状态的任务不该被抢占", status)
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeConflict, code)
			}
		}
	})

	t.Run("错过的触发只补跑最近一次", func(t *testing.T) {
		job := newTestJob(t, 3)
		// 模拟停机两小时：每分钟执行的任务积压了 120 次触发。
		now := job.NextRunAt.Add(2 * time.Hour)
		if _, err := job.ClaimOccurrence(now); err != nil {
			t.Fatalf("抢占失败: %v", err)
		}
		// 新的 NextRunAt 必须是从 now 往后算的，而不是从旧值往后挪一格。
		// 后者会让一次停机被放大成 120 次连续补跑——一场自伤式的雪崩。
		if !job.NextRunAt.After(now) {
			t.Fatalf("NextRunAt 必须从 now 往后算，实际 %s（now=%s）", job.NextRunAt, now)
		}
		if job.NextRunAt.Sub(now) > time.Minute {
			t.Fatalf("下次触发距 now 不应超过一个周期，实际 %s", job.NextRunAt.Sub(now))
		}
	})
}

// ---------------------------------------------------------------------------
// 状态迁移
// ---------------------------------------------------------------------------

func TestPauseResume_RejectIllegalTransitions(t *testing.T) {
	t.Run("重复暂停被拒绝", func(t *testing.T) {
		job := newTestJob(t, 3)
		if err := job.Pause(); err != nil {
			t.Fatalf("首次暂停应当成功: %v", err)
		}
		err := job.Pause()
		if err == nil {
			t.Fatal("重复暂停应被拒绝")
		}
		if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
			t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeConflict, code)
		}
	})

	t.Run("重复恢复被拒绝", func(t *testing.T) {
		job := newTestJob(t, 3)
		err := job.Resume()
		if err == nil {
			t.Fatal("对已启用的任务恢复应被拒绝")
		}
		if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
			t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeConflict, code)
		}
	})

	t.Run("停用的任务不可直接恢复", func(t *testing.T) {
		job := newTestJob(t, 3)
		if err := job.Disable(); err != nil {
			t.Fatalf("停用应当成功: %v", err)
		}
		if err := job.Resume(); err == nil {
			t.Fatal("停用的任务不该被 Resume 直接唤醒")
		}
		if err := job.UpdateSchedule(job.Cron); err == nil {
			t.Fatal("停用的任务不该允许修改调度表达式")
		}
	})

	// 这是「恢复按钮为什么看起来失灵」的那条规则：熔断时计数器停在阈值上，
	// 恢复若不清零，下一次失败会立刻再次熔断。
	t.Run("恢复必须清零失败计数并重算下次触发", func(t *testing.T) {
		const threshold = 2
		job := newTestJob(t, threshold)
		now := time.Now()
		job.RecordFailure("e1", now, "boom", false)
		job.RecordFailure("e2", now, "boom", false)
		job.GetAllPendingEvents()
		if job.Status != value_objects.JobStatusPaused {
			t.Fatalf("前置条件不成立：任务应已熔断，实际 %s", job.Status)
		}

		if err := job.Resume(); err != nil {
			t.Fatalf("恢复应当成功: %v", err)
		}
		if job.ConsecutiveFailures != 0 {
			t.Fatalf("恢复必须清零失败计数，实际 %d", job.ConsecutiveFailures)
		}
		if !job.NextRunAt.After(time.Now()) {
			t.Fatalf("恢复必须按当前时刻重算 NextRunAt，否则会立刻空跑一次，实际 %s", job.NextRunAt)
		}
	})
}

// TestUpdateSchedule_RecomputesNextRun 守住这类系统最经典的静默故障：
// 改了表达式却不重算下次执行时间，界面显示「每小时」而实际还按旧的「每天」跑。
func TestUpdateSchedule_RecomputesNextRun(t *testing.T) {
	job := newTestJob(t, 3)
	// 先把 NextRunAt 推到很远，模拟一条「每天一次」的任务。
	daily, err := value_objects.NewCronExpression("0 3 * * *")
	if err != nil {
		t.Fatalf("构造 cron 失败: %v", err)
	}
	if err := job.UpdateSchedule(daily); err != nil {
		t.Fatalf("更新调度失败: %v", err)
	}
	farAway := job.NextRunAt

	everyMinute, _ := value_objects.NewCronExpression("* * * * *")
	if err := job.UpdateSchedule(everyMinute); err != nil {
		t.Fatalf("更新调度失败: %v", err)
	}
	if !job.NextRunAt.Before(farAway) {
		t.Fatalf("改表达式后必须立刻重算 NextRunAt：got %s, 旧值 %s", job.NextRunAt, farAway)
	}
	if job.Cron.String() != "* * * * *" {
		t.Fatalf("表达式应已更新，实际 %q", job.Cron.String())
	}
}

// TestSchedule_RejectsInvalidInput 守住入口校验。
// 尤其是不可用的 cron：它落库之后是**静默失效**的——任务看起来一切正常，
// 只是永远不会被调度器捞到。
func TestSchedule_RejectsInvalidInput(t *testing.T) {
	valid, _ := value_objects.NewCronExpression("* * * * *")
	empty := value_objects.JobPayload{}

	cases := []struct {
		name string
		call func() (*ScheduledJob, error)
	}{
		{"空 ID", func() (*ScheduledJob, error) {
			return Schedule("", "n", value_objects.JobKindMarketSync, valid, empty, 3, time.Minute, 1)
		}},
		{"空名称", func() (*ScheduledJob, error) {
			return Schedule("id", "  ", value_objects.JobKindMarketSync, valid, empty, 3, time.Minute, 1)
		}},
		{"非法种类", func() (*ScheduledJob, error) {
			return Schedule("id", "n", value_objects.JobKind("bogus"), valid, empty, 3, time.Minute, 1)
		}},
		{"不可用的 cron", func() (*ScheduledJob, error) {
			return Schedule("id", "n", value_objects.JobKindMarketSync,
				value_objects.RehydrateCronExpression("坏掉的表达式"), empty, 3, time.Minute, 1)
		}},
		{"没有创建人", func() (*ScheduledJob, error) {
			return Schedule("id", "n", value_objects.JobKindMarketSync, valid, empty, 3, time.Minute, 0)
		}},
		{"超时超过上限", func() (*ScheduledJob, error) {
			return Schedule("id", "n", value_objects.JobKindMarketSync, valid, empty, 3, MaxTimeout+time.Second, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); err == nil {
				t.Fatal("非法入参必须被拒绝")
			}
		})
	}
}

// TestSchedule_ComputesFirstNextRun 守住「落库即可被调度」：
// 留一个零值 NextRunAt 等着后置流程去补，等于给系统留了一条永不执行的任务。
func TestSchedule_ComputesFirstNextRun(t *testing.T) {
	job := newTestJob(t, 0)
	if job.NextRunAt.IsZero() {
		t.Fatal("构造时必须算出首次 NextRunAt")
	}
	if !job.NextRunAt.After(time.Now().Add(-time.Second)) {
		t.Fatalf("首次 NextRunAt 应在未来，实际 %s", job.NextRunAt)
	}
	if job.Status != value_objects.JobStatusEnabled {
		t.Fatalf("新建任务应为启用状态，实际 %s", job.Status)
	}
	// maxFailures 传 0 时回落到默认阈值，而不是「永不熔断」。
	if job.MaxConsecutiveFailures != DefaultMaxConsecutiveFailures {
		t.Fatalf("未指定阈值应回落到 %d，实际 %d",
			DefaultMaxConsecutiveFailures, job.MaxConsecutiveFailures)
	}
}
