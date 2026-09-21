package entities

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// ===========================================================================
// StateChangedAt 必须跟着每一次状态迁移走
// ===========================================================================
//
// 停滞巡检把「这一行维持当前状态多久了」当作唯一依据。只要有一条迁移路径忘了
// 刷新这个时间戳，巡检就会拿着上一个状态的时刻去判断当前状态——而它的表现是
// 安静的：没有报错、没有日志，只是一个刚刚重新排队的任务会被按老时间戳判成卡死，
// 然后被判失败。
//
// 这条不变式由 setStatus 这个唯一出口来保证，本测试则守住这个保证本身：
// 将来有人新增一条迁移、又绕开 setStatus 直接写 Status，这里会红。

func newTestTask(t *testing.T) *Task {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	tradeDate, err := shared_vo.NewTradeDate(time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("构造交易日失败: %v", err)
	}
	req, err := value_objects.NewRequest(code, tradeDate, value_objects.DepthQuick, nil, "")
	if err != nil {
		t.Fatalf("构造分析请求失败: %v", err)
	}
	task, err := NewTask("task_clock", 1, req)
	if err != nil {
		t.Fatalf("构造任务失败: %v", err)
	}
	return task
}

// 走一遍完整生命周期，每一步都断言时间戳确实前进了。
func TestStateChangedAtAdvancesOnEveryTransition(t *testing.T) {
	task := newTestTask(t)

	if task.StateChangedAt.IsZero() {
		t.Fatal("NewTask 之后 StateChangedAt 不能是零值")
	}

	// 每个步骤：执行迁移 -> 断言状态对 -> 断言时间戳严格前进。
	steps := []struct {
		name string
		do   func() error
		want value_objects.Status
	}{
		{"Start", task.Start, value_objects.StatusRunning},
		{"Fail", func() error { return task.Fail("测试失败", 3) }, value_objects.StatusFailed},
		{"Requeue", task.Requeue, value_objects.StatusQueued},
		{"Start(重试)", task.Start, value_objects.StatusRunning},
		{"Cancel", task.Cancel, value_objects.StatusCanceled},
	}

	for _, step := range steps {
		prev := task.StateChangedAt
		if err := step.do(); err != nil {
			t.Fatalf("%s 失败: %v", step.name, err)
		}
		if task.Status != step.want {
			t.Fatalf("%s 之后状态应为 %s，实际 %s", step.name, step.want, task.Status)
		}
		if !task.StateChangedAt.After(prev) {
			t.Fatalf("%s 之后 StateChangedAt 必须严格前进：迁移前 %s，迁移后 %s",
				step.name, prev, task.StateChangedAt)
		}
	}
}

// Complete 走单独一条路径（它要求非终态 + 非 nil 结果），不能塞进上面的链里。
func TestStateChangedAtAdvancesOnComplete(t *testing.T) {
	task := newTestTask(t)
	if err := task.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	prev := task.StateChangedAt

	if err := task.Complete(&value_objects.Result{}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if task.Status != value_objects.StatusCompleted {
		t.Fatalf("状态应为 completed，实际 %s", task.Status)
	}
	if !task.StateChangedAt.After(prev) {
		t.Fatalf("Complete 之后 StateChangedAt 必须前进：%s -> %s", prev, task.StateChangedAt)
	}
}

// StateChangedAt 与 StartedAt 是两个不同的东西，重试时的行为恰好相反：
// 前者每次都刷新（否则巡检误判），后者永不覆盖（否则 Duration 报的总耗时会缩水）。
// 这两条语义谁被写反了，这里都会红。
func TestStateChangedAtAndStartedAtDivergeOnRetry(t *testing.T) {
	task := newTestTask(t)

	if err := task.Start(); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	firstStartedAt := *task.StartedAt
	firstStateChangedAt := task.StateChangedAt

	if err := task.Fail("第一次失败", 3); err != nil {
		t.Fatalf("Fail 失败: %v", err)
	}
	if err := task.Requeue(); err != nil {
		t.Fatalf("Requeue 失败: %v", err)
	}
	if err := task.Start(); err != nil {
		t.Fatalf("重试 Start 失败: %v", err)
	}

	if !task.StartedAt.Equal(firstStartedAt) {
		t.Fatalf("StartedAt 在重试时不该被覆盖：%s -> %s", firstStartedAt, *task.StartedAt)
	}
	if !task.StateChangedAt.After(firstStateChangedAt) {
		t.Fatalf("StateChangedAt 在重试时必须刷新：%s -> %s",
			firstStateChangedAt, task.StateChangedAt)
	}
	if task.Attempts != 2 {
		t.Fatalf("重试后 Attempts 应为 2，实际 %d", task.Attempts)
	}
}
