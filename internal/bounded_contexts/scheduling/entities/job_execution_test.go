package entities

import (
	"strings"
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// newQueuedExecution 造一条刚排上队、还没被认领的记录。
func newQueuedExecution(t *testing.T) *JobExecution {
	t.Helper()
	exec, err := Enqueue("run_1", "job_1", value_objects.JobKindMarketSync,
		time.Now().Add(-5*time.Second), 1, false)
	if err != nil {
		t.Fatalf("排队失败: %v", err)
	}
	return exec
}

// newRunningExecution 造一条已经被认领、正在执行的记录。
func newRunningExecution(t *testing.T) *JobExecution {
	t.Helper()
	exec := newQueuedExecution(t)
	if err := exec.Start(); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	return exec
}

// ---------------------------------------------------------------------------
// 生命周期：queued -> running -> 终态，不能跳步，不能回头
// ---------------------------------------------------------------------------

func TestEnqueueStartsInQueued(t *testing.T) {
	exec := newQueuedExecution(t)
	if exec.Status != value_objects.ExecutionStatusQueued {
		t.Fatalf("刚排队的记录应为 queued，实际 %s", exec.Status)
	}
	if exec.Attempt != 1 {
		t.Fatalf("首次尝试的 Attempt 应为 1，实际 %d", exec.Attempt)
	}
	if exec.QueuedAt.IsZero() {
		t.Fatal("排队时刻必须落下，否则恢复巡检无从判断它卡了多久")
	}
	// StartedAt 先与 QueuedAt 对齐，让历史列表的「按开始时间倒序」对 queued 记录同样成立。
	if exec.StartedAt.IsZero() {
		t.Fatal("StartedAt 不应为零值，否则这条记录会沉到历史列表最底下")
	}
}

func TestStartRequiresQueued(t *testing.T) {
	// 「只有 queued 才能开始」这条规则必须有唯一的执行点。
	// 注意：并发安全不由实体提供（那是仓储层 ClaimQueued 的职责），
	// 这里守的是调用顺序写错的情况。
	exec := newRunningExecution(t)
	if err := exec.Start(); err == nil {
		t.Fatal("已在执行中的记录不该能再次开始")
	}

	done := newRunningExecution(t)
	if err := done.Succeed("ok", 1); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if err := done.Start(); err == nil {
		t.Fatal("终态记录不该能重新开始")
	}
}

func TestCannotFinishBeforeStart(t *testing.T) {
	// 一条还在 queued 的记录被直接判成终态，意味着它的消息还在队列里，
	// 而历史里已经写着「跑过了」——两边对不上，而且再也对不回来。
	cases := map[string]func(e *JobExecution) error{
		"成功": func(e *JobExecution) error { return e.Succeed("ok", 1) },
		"失败": func(e *JobExecution) error { return e.Fail("上游 500") },
		"跳过": func(e *JobExecution) error { return e.Skip("未注册运行器") },
	}
	for name, close := range cases {
		t.Run(name, func(t *testing.T) {
			exec := newQueuedExecution(t)
			err := close(exec)
			if err == nil {
				t.Fatal("尚未开始的记录不该能收尾")
			}
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeConflict, code)
			}
		})
	}
}

// TestTerminalIsImmutable 守住本聚合的核心不变式：
// 一条审计记录写下结论之后不能翻供，否则「上周那次失败」可能在今天变成成功。
func TestTerminalIsImmutable(t *testing.T) {
	cases := []struct {
		name  string
		close func(e *JobExecution) error
	}{
		{"成功", func(e *JobExecution) error { return e.Succeed("同步完成", 10) }},
		{"失败", func(e *JobExecution) error { return e.Fail("上游 500") }},
		{"跳过", func(e *JobExecution) error { return e.Skip("未注册运行器") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := newRunningExecution(t)
			if err := tc.close(exec); err != nil {
				t.Fatalf("首次收尾应当成功: %v", err)
			}
			if !exec.Status.Terminal() {
				t.Fatalf("收尾后应处于终态，实际 %s", exec.Status)
			}
			if exec.FinishedAt == nil {
				t.Fatal("收尾后必须有结束时刻")
			}
			for _, again := range cases {
				err := again.close(exec)
				if err == nil {
					t.Fatalf("终态记录不该被改写成 %s", again.name)
				}
				if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
					t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeConflict, code)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 重试：新记录而不是改写，且受次数上限约束
// ---------------------------------------------------------------------------

func TestNextAttemptCreatesNewRecord(t *testing.T) {
	// 重试必须是新的一条记录。失败的尝试要原样留在历史里——
	// 重试三次的任务如果只看得到最后一次，前两次为什么失败就无从查起。
	exec := newRunningExecution(t)
	if err := exec.Fail("上游 500"); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}

	next, err := exec.NextAttempt("run_2", 3)
	if err != nil {
		t.Fatalf("派生重试失败: %v", err)
	}
	if next.ID == exec.ID {
		t.Fatal("重试必须是新记录，不能复用原 ID")
	}
	if next.Attempt != exec.Attempt+1 {
		t.Fatalf("尝试次数应递增，实际 %d -> %d", exec.Attempt, next.Attempt)
	}
	if next.Status != value_objects.ExecutionStatusQueued {
		t.Fatalf("新尝试应为 queued，实际 %s", next.Status)
	}
	// 同一次触发的标识必须原样带过去：消费端正是靠它认领这条新记录的。
	if !next.ScheduledFor.Equal(exec.ScheduledFor) || next.JobID != exec.JobID {
		t.Fatal("重试必须属于同一次触发，否则消费端认领不到它")
	}
	// 原记录不受影响。
	if exec.Status != value_objects.ExecutionStatusFailed {
		t.Fatalf("派生重试不该改动原记录，实际 %s", exec.Status)
	}
}

func TestNextAttemptOnlyAfterFailure(t *testing.T) {
	// 成功的没必要重试；跳过的重试也还是跳过（运行器仍然没注册），
	// 只会在历史里刷出一串毫无信息量的记录。
	cases := map[string]func(e *JobExecution) error{
		"成功":  func(e *JobExecution) error { return e.Succeed("ok", 1) },
		"跳过":  func(e *JobExecution) error { return e.Skip("未注册运行器") },
		"执行中": func(e *JobExecution) error { return nil },
	}
	for name, close := range cases {
		t.Run(name, func(t *testing.T) {
			exec := newRunningExecution(t)
			if err := close(exec); err != nil {
				t.Fatalf("收尾失败: %v", err)
			}
			if _, err := exec.NextAttempt("run_2", 3); err == nil {
				t.Fatalf("%s 的记录不该能派生重试", name)
			}
			if exec.CanRetry(3) {
				t.Fatalf("%s 的记录 CanRetry 应为 false", name)
			}
		})
	}
}

func TestNextAttemptRespectsMaxAttempts(t *testing.T) {
	// 没有上限的话，一个永远失败的目标会让同一次触发无限重投。
	exec, err := Enqueue("run_3", "job_1", value_objects.JobKindMarketSync, time.Now(), 3, false)
	if err != nil {
		t.Fatalf("排队失败: %v", err)
	}
	if err := exec.Start(); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if err := exec.Fail("又失败了"); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}

	if exec.CanRetry(3) {
		t.Fatal("已是第 3 次尝试、上限也是 3 时不该再重试")
	}
	if _, err := exec.NextAttempt("run_4", 3); err == nil {
		t.Fatal("超过上限必须被拒绝")
	}
	// 上限放宽就应当允许。
	if !exec.CanRetry(5) {
		t.Fatal("上限放宽到 5 之后，第 3 次尝试应当还能重试")
	}
}

// ---------------------------------------------------------------------------
// 派生量：写路径固化，读路径直接取
// ---------------------------------------------------------------------------

func TestDurationIsFrozen(t *testing.T) {
	exec := newRunningExecution(t)
	if exec.DurationMs != 0 {
		t.Fatalf("未收尾的记录耗时应为 0，实际 %d", exec.DurationMs)
	}
	if err := exec.Succeed("ok", 3); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	frozen := exec.DurationMs
	time.Sleep(5 * time.Millisecond)
	if exec.DurationMs != frozen || exec.Duration() != time.Duration(frozen)*time.Millisecond {
		t.Fatal("耗时必须是收尾时固化的值，不得随时间变化")
	}
}

// TestDelayMeasuresDispatchLag 调度延迟衡量的是「调度器多久之后才发现这次触发」，
// 上限就是巡检间隔。它不该把消息在队列里排的队算进来——那是另一段延迟，
// 混在一起就再也分不清是调度器慢还是消费者不够用。
func TestDelayMeasuresDispatchLag(t *testing.T) {
	scheduledFor := time.Now().Add(-30 * time.Second)
	exec, err := Enqueue("run_2", "job_1", value_objects.JobKindDataCleanup, scheduledFor, 1, false)
	if err != nil {
		t.Fatalf("排队失败: %v", err)
	}
	delay := exec.Delay()
	if delay < 29*time.Second || delay > 31*time.Second {
		t.Fatalf("调度延迟应约为 30s，实际 %s", delay)
	}

	// 在队列里躺一会儿再开始执行，调度延迟不该跟着变。
	time.Sleep(10 * time.Millisecond)
	if err := exec.Start(); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if after := exec.Delay(); after != delay {
		t.Fatalf("开始执行不该改变调度延迟，%s -> %s", delay, after)
	}
}

// TestQueueWaitMeasuresBacklog 队列等待是消息驱动之后新出现的一段延迟，
// 也是最值得盯的一段：它变大说明消费者不够用，而调度延迟对此毫无反应。
func TestQueueWaitMeasuresBacklog(t *testing.T) {
	exec := newQueuedExecution(t)
	if exec.QueueWait() != 0 {
		t.Fatal("还在排队的记录，队列等待时长应为 0（它还没等完）")
	}

	time.Sleep(10 * time.Millisecond)
	if err := exec.Start(); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if wait := exec.QueueWait(); wait < 5*time.Millisecond {
		t.Fatalf("队列等待时长应当反映真实等待，实际 %s", wait)
	}
}

// ---------------------------------------------------------------------------
// 入参与列宽
// ---------------------------------------------------------------------------

func TestEnqueueRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		jobID string
		sched time.Time
	}{
		{"空 ID", "", "job_1", time.Now()},
		{"空任务 ID", "run_1", "", time.Now()},
		{"没有计划触发时刻", "run_1", "job_1", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Enqueue(tc.id, tc.jobID, value_objects.JobKindMarketSync, tc.sched, 1, false)
			if err == nil {
				t.Fatal("非法入参必须被拒绝")
			}
		})
	}
}

// TestScheduledForAlignsToMillisecond 守住一个会让触发静默消失的细节：
// scheduled_for 参与唯一索引，也是消费端认领时 WHERE 里的等值条件，
// 而库里的列是 datetime(3)。不对齐的话那条等值查询永远查不到。
func TestScheduledForAlignsToMillisecond(t *testing.T) {
	odd := time.Date(2026, 9, 17, 10, 0, 0, 123_456_789, time.Local)
	exec, err := Enqueue("run_1", "job_1", value_objects.JobKindMarketSync, odd, 1, false)
	if err != nil {
		t.Fatalf("排队失败: %v", err)
	}
	if exec.ScheduledFor.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("计划时刻必须对齐到毫秒，实际 %d ns", exec.ScheduledFor.Nanosecond())
	}
	// 重试必须落在完全相同的时刻上，否则它就属于「另一次触发」了。
	if err := exec.Start(); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if err := exec.Fail("boom"); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	next, err := exec.NextAttempt("run_2", 3)
	if err != nil {
		t.Fatalf("派生重试失败: %v", err)
	}
	if !next.ScheduledFor.Equal(exec.ScheduledFor) {
		t.Fatalf("重试的计划时刻必须与原尝试完全一致，%s vs %s", next.ScheduledFor, exec.ScheduledFor)
	}
}

// TestSummaryTruncation 守住列宽对齐。运行器的摘要可能来自上游的错误文案，
// 长度完全不可控；在领域层截断好过让一次保存变成一个数据库报错。
func TestSummaryTruncation(t *testing.T) {
	exec := newRunningExecution(t)
	// 用汉字构造超长摘要，顺带验证不会把多字节字符切成半个。
	long := strings.Repeat("同步", 500)
	if err := exec.Succeed(long, 1); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if len(exec.Summary) > maxSummaryLen {
		t.Fatalf("摘要应被截断到 %d 字节以内，实际 %d", maxSummaryLen, len(exec.Summary))
	}
	if !utf8Valid(exec.Summary) {
		t.Fatal("截断不得产生非法 UTF-8 序列")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
