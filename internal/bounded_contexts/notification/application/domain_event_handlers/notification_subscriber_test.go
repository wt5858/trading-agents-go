package domain_event_handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	analysis_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/value_objects"
	scheduling_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_events"
	stock_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_events"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ===========================================================================
// 测试替身
// ===========================================================================

// fakeNotifier 是 NotificationService 的内存替身，它只需要精确复刻一件事：
// **(user_id, dedupe_key) 上的唯一索引**。
//
// 这里刻意复用真正的 entities.Notify 来推导去重键，而不是在测试里自己拼一个：
// 本测试要验证的是「同一个事件第二次到达不会产出第二条通知」，如果连键都是
// 测试自己算的，那验证的就只是测试自己前后一致，与生产行为无关。
//
// 唯一索引本身用一张 map 表达——机制不同（真库靠的是索引），语义相同：
// 同一个键第二次写入必然失败，且失败的错误码是 AlreadyExists（仓储对 1062 的翻译）。
type fakeNotifier struct {
	rows  map[string]*entities.Notification
	order []*entities.Notification
	calls int

	// failWith 非空时，本替身对任何调用都返回该错误，用于测降级分支。
	failWith error
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{rows: make(map[string]*entities.Notification)}
}

func (f *fakeNotifier) Notify(_ context.Context, in domain_services.NotifyInput) (*entities.Notification, error) {
	f.calls++
	if f.failWith != nil {
		return nil, f.failWith
	}
	n, err := entities.Notify(entities.NotifyParams{
		UserID:   in.UserID,
		Kind:     in.Kind,
		Level:    in.Level,
		Title:    in.Title,
		Body:     in.Body,
		SourceID: in.SourceID,
		LinkType: in.LinkType,
		LinkID:   in.LinkID,
	})
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%d|%s", n.UserID, n.DedupeKey.String())
	if _, exists := f.rows[key]; exists {
		return nil, custom_errors.AlreadyExists("用户(id=%d) 的通知(去重键=%s) 已存在", n.UserID, n.DedupeKey.String())
	}
	f.rows[key] = n
	f.order = append(f.order, n)
	return n, nil
}

func (f *fakeNotifier) count() int { return len(f.order) }

// fakeBus 复刻 AmqpBus 的注册与分发，但**不经过序列化**，也不做重试。
// 测试关心的是「处理器收到事件后返回了什么」，而那与信封长什么样无关；
// 用真实总线就得先起一个 broker，把一组纯逻辑断言变成集成测试。
type fakeBus struct {
	handlers map[string][]domain_event.Handler
}

func newFakeBus() *fakeBus { return &fakeBus{handlers: make(map[string][]domain_event.Handler)} }

// RegisterSubscriber 与真实总线同签名：事件名从样例事件上取，
// 这样「订阅的名字」和「发出的名字」不可能对不上。
func (b *fakeBus) RegisterSubscriber(h domain_event.Handler, prototype domain_event.DomainEvent) {
	name := prototype.Name()
	b.handlers[name] = append(b.handlers[name], h)
}

// dispatch 把一个事件投给它的全部订阅者，返回第一个非 nil 的错误。
func (b *fakeBus) dispatch(t *testing.T, e domain_event.DomainEvent) error {
	t.Helper()
	hs := b.handlers[e.Name()]
	if len(hs) == 0 {
		t.Fatalf("事件 %s 没有任何订阅者", e.Name())
	}
	for _, h := range hs {
		if err := h(context.Background(), e); err != nil {
			return err
		}
	}
	return nil
}

// newSubscribedBus 组装一个挂好订阅的总线。
func newSubscribedBus(notifier Notifier) *fakeBus {
	bus := newFakeBus()
	NewNotificationSubscriber(notifier, nil).Register(bus)
	return bus
}

// ===========================================================================
// 幂等：同一个事件到达两次，只产出一条通知，且两次都不报错
// ===========================================================================

func TestOnTaskCompleted_重复投递只产出一条通知且不报错(t *testing.T) {
	const (
		userID = uint64(42)
		taskID = "task-abc"
	)
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	evt := analysis_events.NewOnTaskCompleted(taskID, userID, "", "600519.SH", "buy", decimal.RequireFromString("0.82"), decimal.RequireFromString("12.5"))

	// 第一次投递：正常写入。
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("首次投递不该失败: %v", err)
	}
	// 第二次投递同一个事件对象：总线重启后的补投就是这个样子。
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("重放不该报错——它撞上的是唯一索引，而那正是我们想要的结果: %v", err)
	}
	// 第三次：**重新构造**的事件，EventID 是全新的 uuid，但描述的是同一件事。
	// 这一条才是关键：若去重键掺了事件 ID 或时间，前两次能过，这一次必然多出一条。
	replay := analysis_events.NewOnTaskCompleted(taskID, userID, "", "600519.SH", "buy", decimal.RequireFromString("0.82"), decimal.RequireFromString("12.5"))
	if replay.EventID() == evt.EventID() {
		t.Fatal("前提不成立：重新构造的事件应当有不同的 EventID")
	}
	if err := bus.dispatch(t, replay); err != nil {
		t.Fatalf("换了 EventID 的重放同样不该报错: %v", err)
	}

	if got := notifier.count(); got != 1 {
		t.Fatalf("产出 %d 条通知，期望 1 条——重放必须是无操作", got)
	}
	if notifier.calls != 3 {
		t.Fatalf("Notify 调用了 %d 次，期望 3 次：处理器不该自作主张地跳过投递", notifier.calls)
	}

	n := notifier.order[0]
	if n.UserID != userID {
		t.Fatalf("收件人 = %d，期望 %d", n.UserID, userID)
	}
	if n.Kind != value_objects.KindAnalysisCompleted {
		t.Fatalf("种类 = %s，期望 %s", n.Kind, value_objects.KindAnalysisCompleted)
	}
	if n.Level != value_objects.LevelInfo {
		t.Fatalf("级别 = %s，期望 %s（分析完成是告知，不是告警）", n.Level, value_objects.LevelInfo)
	}
	if n.LinkType != value_objects.LinkAnalysisTask || n.LinkID != taskID {
		t.Fatalf("跳转目标 = (%s, %s)，期望 (%s, %s)",
			n.LinkType, n.LinkID, value_objects.LinkAnalysisTask, taskID)
	}
	if !strings.Contains(n.Title, "600519.SH") {
		t.Fatalf("标题 %q 里没有标的代码，用户无从知道是哪只票完成了", n.Title)
	}
	if !strings.Contains(n.Body, "买入") {
		t.Fatalf("正文 %q 没有把 action 译成中文", n.Body)
	}
}

func TestOnTaskFailed_重复投递同样只产出一条通知(t *testing.T) {
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	evt := analysis_events.NewOnTaskFailed("task-x", 7, "", "数据源连续超时", 3, false)
	for i := 0; i < 3; i++ {
		if err := bus.dispatch(t, evt); err != nil {
			t.Fatalf("第 %d 次投递失败: %v", i+1, err)
		}
	}
	if got := notifier.count(); got != 1 {
		t.Fatalf("产出 %d 条通知，期望 1 条", got)
	}
	n := notifier.order[0]
	if n.Level != value_objects.LevelWarning {
		t.Fatalf("级别 = %s，期望 %s", n.Level, value_objects.LevelWarning)
	}
	if !strings.Contains(n.Body, "数据源连续超时") {
		t.Fatalf("正文 %q 里没有失败原因", n.Body)
	}
}

func TestOnTaskFailed_可重试的失败不发通知(t *testing.T) {
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	// Retryable：这次失败之后还会自动重试，很可能下一次就成功了。
	evt := analysis_events.NewOnTaskFailed("task-y", 7, "", "临时限流", 1, true)
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if got := notifier.count(); got != 0 {
		t.Fatalf("产出了 %d 条通知，期望 0 条：中途重试不该打扰用户，"+
			"而且它会占掉去重键，把后面真正的终局失败挡在门外", got)
	}
	if notifier.calls != 0 {
		t.Fatalf("Notify 被调用了 %d 次，期望 0 次", notifier.calls)
	}
}

func TestOnTaskCompleted_与_OnTaskFailed_互不覆盖(t *testing.T) {
	const taskID = "task-both"
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	// 同一个任务先失败、重试后成功：两件事，两条通知。
	if err := bus.dispatch(t, analysis_events.NewOnTaskFailed(taskID, 7, "", "第一次失败", 1, false)); err != nil {
		t.Fatalf("投递失败通知出错: %v", err)
	}
	if err := bus.dispatch(t, analysis_events.NewOnTaskCompleted(taskID, 7, "", "600519.SH", "hold", decimal.RequireFromString("0.6"), decimal.RequireFromString("30"))); err != nil {
		t.Fatalf("投递完成通知出错: %v", err)
	}
	if got := notifier.count(); got != 2 {
		t.Fatalf("产出 %d 条通知，期望 2 条：种类进了去重键，两者不该互相挡住", got)
	}
}

// ===========================================================================
// 运维事件：没有收件人就不发，也绝不编一个出来
// ===========================================================================

func TestOnSyncFailed_不产出通知且不报错(t *testing.T) {
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	evt := stock_events.NewOnSyncFailed("run-1", "quote", "CN", "数据源 500")
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// 事件载荷里没有任何用户身份。凭空写一条通知，就意味着 user_id 是猜出来的，
	// 而它还会因为去重键固定而无法被那个无辜用户清理干净。
	if notifier.calls != 0 {
		t.Fatalf("Notify 被调用了 %d 次，期望 0 次：这条事件没有收件人", notifier.calls)
	}
}

func TestOnJobAutoPaused_不产出通知且不报错(t *testing.T) {
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	evt := scheduling_events.NewOnJobAutoPaused("job-1", "行情日更", "market_sync", 5, "连续超时")
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if notifier.calls != 0 {
		t.Fatalf("Notify 被调用了 %d 次，期望 0 次：这条事件没有收件人", notifier.calls)
	}
}

func TestDeliver_事件缺少收件人时跳过而不是报错(t *testing.T) {
	notifier := newFakeNotifier()
	bus := newSubscribedBus(notifier)

	// UserID 为 0 的分析完成事件：发布方的问题，但不该变成一次无限重投。
	evt := analysis_events.NewOnTaskCompleted("task-z", 0, "", "600519.SH", "buy", decimal.RequireFromString("0.5"), decimal.RequireFromString("1"))
	if err := bus.dispatch(t, evt); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if notifier.calls != 0 {
		t.Fatalf("Notify 被调用了 %d 次，期望 0 次", notifier.calls)
	}
}

// ===========================================================================
// 降级：什么错误值得重投，什么不值得
// ===========================================================================

func TestDeliver_构造类错误被吞掉_基础设施错误被上抛(t *testing.T) {
	cases := []struct {
		name      string
		failWith  error
		wantError bool
	}{
		{
			name:      "重放撞上唯一索引，视为成功",
			failWith:  custom_errors.AlreadyExists("通知已存在"),
			wantError: false,
		},
		{
			name:      "参数非法，重投一百次也一样，吞掉但要留日志",
			failWith:  custom_errors.Invalid("通知标题不能为空"),
			wantError: false,
		},
		{
			name:      "状态冲突，同样不会因重投而变好",
			failWith:  custom_errors.Conflict("通知已是已读状态"),
			wantError: false,
		},
		{
			name:      "数据库不可用，可能是暂时的，如实上抛",
			failWith:  custom_errors.Internal("数据库操作失败"),
			wantError: true,
		},
		{
			name:      "依赖不可用，同样值得重投",
			failWith:  custom_errors.Unavailable("连接超时"),
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newFakeNotifier()
			notifier.failWith = tc.failWith
			bus := newSubscribedBus(notifier)

			err := bus.dispatch(t, analysis_events.NewOnTaskCompleted("task-1", 9, "", "600519.SH", "buy", decimal.RequireFromString("0.7"), decimal.RequireFromString("5")))
			if tc.wantError && err == nil {
				t.Fatal("期望把错误交还给总线，好让它被记录、被重投")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("不该上抛: %v", err)
			}
		})
	}
}

// ===========================================================================
// 装配
// ===========================================================================

func TestRegister_订阅了全部四个事件(t *testing.T) {
	bus := newSubscribedBus(newFakeNotifier())

	want := []string{
		analysis_events.OnTaskCompletedEventName,
		analysis_events.OnTaskFailedEventName,
		stock_events.OnSyncFailedEventName,
		scheduling_events.OnJobAutoPausedEventName,
	}
	for _, name := range want {
		if len(bus.handlers[name]) != 1 {
			t.Fatalf("事件 %s 的订阅者数量 = %d，期望 1", name, len(bus.handlers[name]))
		}
	}
	if len(bus.handlers) != len(want) {
		t.Fatalf("订阅了 %d 类事件，期望 %d 类", len(bus.handlers), len(want))
	}
}

// TestHandlers_订阅错事件名时静默忽略。
//
// 装配错误（把 OnTaskCompleted 的处理器挂到别的事件上）不该在运行期把总线搞崩：
// 一个类型断言失败的处理器返回 nil，其余订阅者照常工作。
func TestHandlers_收到不认识的事件时静默忽略(t *testing.T) {
	notifier := newFakeNotifier()
	s := NewNotificationSubscriber(notifier, nil)
	alien := stock_events.NewOnSyncCompleted("run-1", "quote", "CN", "ok", 10, 10, 0, decimal.NewFromInt(1))

	handlers := map[string]domain_event.Handler{
		"OnTaskCompleted": s.OnTaskCompleted,
		"OnTaskFailed":    s.OnTaskFailed,
		"OnSyncFailed":    s.OnSyncFailed,
		"OnJobAutoPaused": s.OnJobAutoPaused,
	}
	for name, h := range handlers {
		if err := h(context.Background(), alien); err != nil {
			t.Fatalf("%s 收到不认识的事件时不该报错: %v", name, err)
		}
	}
	if notifier.calls != 0 {
		t.Fatalf("Notify 被调用了 %d 次，期望 0 次", notifier.calls)
	}
}
