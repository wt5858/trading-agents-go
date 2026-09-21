package domain_event_handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	analysis_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// fakeGenerator 复刻真实链路的关键行为：唯一索引挡住第二次写入。
//
// 它不模拟数据库，只模拟那一条唯一约束——因为处理器的幂等性完全建立在
// 「重复的 task_id 会拿到 AlreadyExists」这一个事实上，其余都是噪音。
type fakeGenerator struct {
	stored   map[string]string // taskID -> reportID，代表库里真实存在的行
	calls    int
	failWith error
}

func newFakeGenerator() *fakeGenerator {
	return &fakeGenerator{stored: make(map[string]string)}
}

func (f *fakeGenerator) Generate(_ context.Context, in domain_services.GenerateInput) (*entities.Report, error) {
	f.calls++
	if f.failWith != nil {
		return nil, f.failWith
	}
	if _, exists := f.stored[in.TaskID]; exists {
		// 这正是 ReportRepository.Create 撞唯一键后经 translate 抛出的错误。
		return nil, custom_errors.AlreadyExists("分析任务(id=%s) 的报告 已存在", in.TaskID)
	}
	// 每次尝试都会分配一个新的报告 ID；幂等建立在 task_id 上，不在主键上。
	reportID := "rpt_" + in.TaskID
	f.stored[in.TaskID] = reportID
	return &entities.Report{ID: reportID, TaskID: in.TaskID, UserID: in.UserID}, nil
}

func completedEvent(taskID string) *analysis_events.OnTaskCompleted {
	return analysis_events.NewOnTaskCompleted(taskID, 7, "", "600519.SH", "buy", decimal.RequireFromString("0.82"), decimal.RequireFromString("12.5"))
}

// TestOnTaskCompleted_IsIdempotent 是本处理器存在的核心契约。
//
// 领域事件是至少一次投递的：总线重启补投、worker 可见性超时、以后换 MQ 后的重投，
// 都会让同一个 OnTaskCompleted 到达不止一次。重放必须满足两件事——
// 不报错，也不产出第二份报告。
func TestOnTaskCompleted_IsIdempotent(t *testing.T) {
	gen := newFakeGenerator()
	h := NewOnTaskCompletedHandler(gen)
	evt := completedEvent("task_1")
	ctx := context.Background()

	// 首次投递：正常生成。
	if err := h.OnTaskCompleted(ctx, evt); err != nil {
		t.Fatalf("首次投递不应失败: %v", err)
	}

	// 重放同一个事件多次，模拟补投与重试。
	for i := 0; i < 5; i++ {
		if err := h.OnTaskCompleted(ctx, evt); err != nil {
			t.Fatalf("第 %d 次重放不应失败（AlreadyExists 必须被当成成功）: %v", i+2, err)
		}
	}

	if gen.calls != 6 {
		t.Fatalf("处理器不该自行去重（那是 TOCTOU），应每次都尝试写入，实际调用 %d 次", gen.calls)
	}
	if len(gen.stored) != 1 {
		t.Fatalf("同一个任务只应有一份报告，实际 %d 份", len(gen.stored))
	}
	if gen.stored["task_1"] != "rpt_task_1" {
		t.Fatalf("库里应保留最先写入的那份报告，实际 %q", gen.stored["task_1"])
	}
}

// TestOnTaskCompleted_DistinctTasksStillGenerate 幂等不能退化成「只生成一次就不干活了」。
func TestOnTaskCompleted_DistinctTasksStillGenerate(t *testing.T) {
	gen := newFakeGenerator()
	h := NewOnTaskCompletedHandler(gen)
	ctx := context.Background()

	for _, taskID := range []string{"task_1", "task_2", "task_1", "task_3"} {
		if err := h.OnTaskCompleted(ctx, completedEvent(taskID)); err != nil {
			t.Fatalf("任务 %s 投递失败: %v", taskID, err)
		}
	}
	if len(gen.stored) != 3 {
		t.Fatalf("三个不同任务应各出一份报告，实际 %d 份", len(gen.stored))
	}
}

// TestOnTaskCompleted_PropagatesRealErrors 只有 AlreadyExists 被当成成功。
// 真正的故障（结论查不到、数据库挂了）必须照实上抛，否则一次持续的故障
// 会被伪装成「处理成功」，报告永远不会被生成，也不会有任何告警。
func TestOnTaskCompleted_PropagatesRealErrors(t *testing.T) {
	cases := map[string]error{
		"分析结论不存在": custom_errors.NotFound("分析任务(id=task_1) 尚未产出结论"),
		"数据库不可用":  custom_errors.Internal("数据库操作失败"),
		"非领域错误":   errors.New("connection refused"),
	}
	for name, wantErr := range cases {
		t.Run(name, func(t *testing.T) {
			gen := newFakeGenerator()
			gen.failWith = wantErr
			h := NewOnTaskCompletedHandler(gen)

			err := h.OnTaskCompleted(context.Background(), completedEvent("task_1"))
			if err == nil {
				t.Fatal("真实故障必须上抛，实际被吞掉了")
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("应原样上抛底层错误，实际 %v", err)
			}
		})
	}
}

// TestOnTaskCompleted_IgnoresForeignEvents 订阅错事件名是装配错误，
// 静默忽略即可，不该把总线搞崩。
func TestOnTaskCompleted_IgnoresForeignEvents(t *testing.T) {
	gen := newFakeGenerator()
	h := NewOnTaskCompletedHandler(gen)

	other := analysis_events.NewOnTaskFailed("task_1", 7, "", "执行失败", 1, false)
	if err := h.OnTaskCompleted(context.Background(), other); err != nil {
		t.Fatalf("非预期事件应被忽略: %v", err)
	}
	if gen.calls != 0 {
		t.Fatalf("非预期事件不该触发生成，实际调用 %d 次", gen.calls)
	}
}

// TestRegister 处理器必须挂在 analysis.task_completed 上。
// 这个名字是两个上下文之间的契约，写错了整条链路会静默不工作。
func TestRegister(t *testing.T) {
	bus := &fakeBus{handlers: make(map[string][]domain_event.Handler)}
	NewOnTaskCompletedHandler(newFakeGenerator()).Register(bus)

	if len(bus.handlers[analysis_events.OnTaskCompletedEventName]) != 1 {
		t.Fatalf("应订阅 %s，实际订阅情况 %v",
			analysis_events.OnTaskCompletedEventName, bus.handlers)
	}
}

type fakeBus struct {
	handlers map[string][]domain_event.Handler
}

// RegisterSubscriber 与真实总线同签名：事件名从样例事件上取，
// 这样「订阅的名字」和「发出的名字」不可能对不上。
func (b *fakeBus) RegisterSubscriber(h domain_event.Handler, prototype domain_event.DomainEvent) {
	name := prototype.Name()
	b.handlers[name] = append(b.handlers[name], h)
}
