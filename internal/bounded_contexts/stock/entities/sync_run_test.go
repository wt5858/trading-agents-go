package entities

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

func newRun(t *testing.T) *SyncRun {
	t.Helper()
	run, err := StartSyncRun("sync_1", value_objects.SyncQuotes, shared_vo.MarketCN, "test")
	if err != nil {
		t.Fatalf("StartSyncRun: %v", err)
	}
	return run
}

// Finish 的终态判定是业务规则，必须由聚合决定，所以要把三条分支都钉住。
func TestFinishClassifiesOutcomeFromStats(t *testing.T) {
	cases := []struct {
		name  string
		stats value_objects.SyncStats
		want  value_objects.SyncStatus
	}{
		{"全部成功", value_objects.NewSyncStats(100, 100, 0, 0), value_objects.SyncSucceeded},
		{"部分失败即部分成功", value_objects.NewSyncStats(100, 88, 12, 0), value_objects.SyncPartial},
		{"颗粒无收判失败", value_objects.NewSyncStats(100, 0, 100, 0), value_objects.SyncFailed},
		// 没有标的可同步不是失败：它和「全都失败了」是两回事。
		{"零标的算成功", value_objects.NewSyncStats(0, 0, 0, 0), value_objects.SyncSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun(t)
			if err := run.Finish(tc.stats); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if run.Status != tc.want {
				t.Fatalf("状态 = %s，期望 %s", run.Status, tc.want)
			}
			if run.FinishedAt == nil {
				t.Fatal("终态必须记录结束时间")
			}
			if len(run.GetAllPendingEvents()) == 0 {
				t.Fatal("终态必须登记领域事件")
			}
		})
	}
}

func TestTerminalRunRejectsFurtherTransitions(t *testing.T) {
	run := newRun(t)
	if err := run.Finish(value_objects.NewSyncStats(1, 1, 0, 0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := run.Finish(value_objects.NewSyncStats(1, 1, 0, 0)); err == nil {
		t.Fatal("重复 Finish 必须被拒绝")
	}
	if err := run.Advance("000001", 1, 0, 0); err == nil {
		t.Fatal("终态后不允许继续推进")
	}
	if err := run.Cancel(); err == nil {
		t.Fatal("终态后不允许取消")
	}
}

func TestSuccessRateComputedOnceAndPersistedShape(t *testing.T) {
	s := value_objects.NewSyncStats(3, 1, 2, 0)
	// 1/3 在 float64 下是 33.333333333333336，取两位小数还要靠手写的
	// int64(x*100+0.5) 兜一层。decimal 直接给出 33.33，且与 decimal(5,2) 列等价。
	if got := s.SuccessRate; !got.Equal(decimal.RequireFromString("33.33")) {
		t.Fatalf("成功率 = %v，期望 33.33", got)
	}
	// 读回不得重算：库里存的才是当时的事实。
	rehydrated := value_objects.RehydrateSyncStats(3, 1, 2, 0, decimal.RequireFromString("99.99"))
	if !rehydrated.SuccessRate.Equal(decimal.RequireFromString("99.99")) {
		t.Fatalf("读回应保留存量成功率，得到 %v", rehydrated.SuccessRate)
	}
	// 分母为 0 不能产生 NaN——NaN 会让 JSON 序列化直接失败。
	// decimal 没有 NaN，但除零会 panic，所以零总量必须在进除法之前短路。
	if zero := value_objects.NewSyncStats(0, 0, 0, 0); !zero.SuccessRate.IsZero() {
		t.Fatalf("零总量成功率应为 0，得到 %v", zero.SuccessRate)
	}
}

func TestAdvanceAccumulatesAndMovesCursor(t *testing.T) {
	run := newRun(t)
	if err := run.PlanTotal(10); err != nil {
		t.Fatalf("PlanTotal: %v", err)
	}
	if err := run.Advance("000005", 4, 1, 0); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := run.Advance("000009", 3, 2, 0); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Cursor != "000009" {
		t.Fatalf("游标 = %s，期望 000009", run.Cursor)
	}
	if run.Stats.Succeeded != 7 || run.Stats.Failed != 3 {
		t.Fatalf("统计累加错误: %+v", run.Stats)
	}
	if run.Stats.Total != 10 {
		t.Fatalf("总量应保持 10，得到 %d", run.Stats.Total)
	}
}

// running_key 是「同类同市场只允许一个在跑」这条不变式的数据库载体，
// 终态必须让出占位，否则同类同步再也起不来。
func TestRunningKeyReleasedOnTerminal(t *testing.T) {
	run := newRun(t)
	if run.RunningKey() != "quotes:CN" {
		t.Fatalf("运行中 key = %q，期望 quotes:CN", run.RunningKey())
	}
	if err := run.Finish(value_objects.NewSyncStats(1, 1, 0, 0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if run.RunningKey() != "" {
		t.Fatalf("终态 key 必须为空，得到 %q", run.RunningKey())
	}
}

func TestResumableOnlyForInterruptedRuns(t *testing.T) {
	cases := []struct {
		name   string
		finish func(*SyncRun)
		cursor string
		want   bool
	}{
		{"成功不需要续传", func(r *SyncRun) { _ = r.Finish(value_objects.NewSyncStats(1, 1, 0, 0)) }, "000001", false},
		{"失败且有断点可续传", func(r *SyncRun) { _ = r.Fail("数据源超时") }, "000001", true},
		{"取消且有断点可续传", func(r *SyncRun) { _ = r.Cancel() }, "000001", true},
		{"没有断点无从续起", func(r *SyncRun) { _ = r.Fail("启动即失败") }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun(t)
			run.Cursor = tc.cursor
			tc.finish(run)
			if got := run.Resumable(); got != tc.want {
				t.Fatalf("Resumable = %v，期望 %v", got, tc.want)
			}
		})
	}
}
