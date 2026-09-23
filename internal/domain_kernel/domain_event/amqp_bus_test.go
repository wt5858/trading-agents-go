package domain_event

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// 本文件覆盖总线自己的那部分：信封的形状、消费端的类型还原、
// 以及最要紧的一件事——什么时候确认丢弃、什么时候上抛触发重投。
// 这个判断错了，要么是消息在重试队列里无意义地兜圈子，要么是该重试的被丢了。

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

type capturingPublisher struct {
	sent []string
	err  error
}

func (p *capturingPublisher) PublishDirectMessageWithContext(_ context.Context, _, _, body string) error {
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, body)
	return nil
}

// probeEvent 是一个只在测试里存在的事件，带一个字段用来验证反序列化确实发生了。
type probeEvent struct {
	BaseDomainEvent
	Subject string `json:"subject"`
}

func newProbeEvent(subject string) *probeEvent {
	return &probeEvent{BaseDomainEvent: NewBaseDomainEvent(), Subject: subject}
}

func (e *probeEvent) Name() string { return "test.probe" }

func (e *probeEvent) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// otherEvent 用来验证「本进程没订阅的事件」这条路径。
type otherEvent struct{ BaseDomainEvent }

func newOtherEvent() *otherEvent { return &otherEvent{NewBaseDomainEvent()} }

func (e *otherEvent) Name() string { return "test.other" }

func (e *otherEvent) ToJson() (string, error) {
	b, err := json.Marshal(e)
	return string(b), err
}

// ---------------------------------------------------------------------------
// 发布
// ---------------------------------------------------------------------------

func TestPublishWrapsEventInEnvelope(t *testing.T) {
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	event := newProbeEvent("行情同步")
	if err := bus.Publish(context.Background(), event); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if len(pub.sent) != 1 {
		t.Fatalf("应当发出 1 条消息，实际 %d 条", len(pub.sent))
	}

	var env envelope
	if err := json.Unmarshal([]byte(pub.sent[0]), &env); err != nil {
		t.Fatalf("信封无法解析: %v", err)
	}
	// 信封上的这三项是给人看的：运维在 broker 管理界面翻一条积压消息时，
	// 不该需要先知道类型才能看懂它是什么。
	if env.Name != "test.probe" {
		t.Fatalf("信封上的事件名 = %q", env.Name)
	}
	if env.EventID != event.EventID() {
		t.Fatalf("信封上的事件 ID = %q，want %q", env.EventID, event.EventID())
	}
	if env.OccurredAt.IsZero() {
		t.Fatal("信封上应当带有发生时刻")
	}
	if !strings.Contains(string(env.Payload), "行情同步") {
		t.Fatalf("载荷里应当保留事件本身的字段，实际: %s", env.Payload)
	}
}

func TestPublishSendsEachEventSeparately(t *testing.T) {
	// 逐条发而不是打包：打包会让其中一个事件的处理失败拖着其余事件一起重投，
	// 而它们之间本来没有任何关系。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	if err := bus.Publish(context.Background(), newProbeEvent("a"), newProbeEvent("b")); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if len(pub.sent) != 2 {
		t.Fatalf("两个事件应当是两条消息，实际 %d 条", len(pub.sent))
	}
}

func TestPublishReportsBrokerFailure(t *testing.T) {
	// 发布失败必须让调用方知道：领域决策已经落库，但没人会收到这个事实，
	// 静默吞掉等于让一条断掉的链路看起来完全正常。
	sentinel := errors.New("broker 不可用")
	bus := NewAmqpBus(&capturingPublisher{err: sentinel}, "exchange.x", "rk", nil)

	err := bus.Publish(context.Background(), newProbeEvent("a"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("应当上抛 broker 的错误，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 消费
// ---------------------------------------------------------------------------

// publishAndDispatch 走一遍完整的「发出去再收回来」，这正是真实链路的形状。
func publishAndDispatch(t *testing.T, bus *AmqpBus, pub *capturingPublisher, e DomainEvent) error {
	t.Helper()
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	return bus.Dispatch(context.Background(), pub.sent[len(pub.sent)-1])
}

func TestDispatchRestoresConcreteEventType(t *testing.T) {
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	var got *probeEvent
	bus.RegisterSubscriber(func(_ context.Context, e DomainEvent) error {
		got, _ = e.(*probeEvent)
		return nil
	}, &probeEvent{})

	if err := publishAndDispatch(t, bus, pub, newProbeEvent("行情同步")); err != nil {
		t.Fatalf("分发失败: %v", err)
	}
	if got == nil {
		t.Fatal("处理器应当拿到还原后的具体事件类型，而不是一串 JSON")
	}
	if got.Subject != "行情同步" {
		t.Fatalf("字段没有还原，Subject = %q", got.Subject)
	}
	if got.EventID() == "" || got.OccurredAt().IsZero() {
		t.Fatal("内嵌的 BaseDomainEvent 也应当被还原")
	}
}

// recorder 是一个有名字的处理器：名字必须来自方法值，
// 因为总线正是用方法值推导处理器名的。
type recorder struct {
	runs int
	err  error
}

func (r *recorder) Handle(context.Context, DomainEvent) error {
	r.runs++
	return r.err
}

type otherRecorder struct {
	runs int
	err  error
}

func (r *otherRecorder) Handle(context.Context, DomainEvent) error {
	r.runs++
	return r.err
}

type thirdRecorder struct {
	runs int
	err  error
}

func (r *thirdRecorder) Handle(context.Context, DomainEvent) error {
	r.runs++
	return r.err
}

func TestDispatchRunsEveryHandlerEvenWhenOneFails(t *testing.T) {
	// 遇到第一个错误就返回，会让后面的处理器在本轮完全没有机会执行。
	// 团队既有服务正是这么做的，而它留下一个洞：那些没跑的处理器在重投时
	// 又因为名字对不上被跳过，于是一次都不会执行，且没有任何错误信号。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	first := &recorder{err: errors.New("写通知失败")}
	second := &otherRecorder{}
	bus.RegisterSubscriber(first.Handle, &probeEvent{})
	bus.RegisterSubscriber(second.Handle, &probeEvent{})

	err := publishAndDispatch(t, bus, pub, newProbeEvent("a"))
	if second.runs != 1 {
		t.Fatalf("前一个处理器失败不该让后一个失去执行机会，实际执行 %d 次", second.runs)
	}
	if err == nil {
		t.Fatal("处理器的失败应当上抛以触发重投")
	}
}

// ---------------------------------------------------------------------------
// 失败隔离：一个处理器失败，重投时只重跑它
// ---------------------------------------------------------------------------

func TestOnlyFailedHandlerIsRetried(t *testing.T) {
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	ok1 := &recorder{}
	bad := &otherRecorder{err: errors.New("下游暂时不可用")}
	ok2 := &thirdRecorder{}
	bus.RegisterSubscriber(ok1.Handle, &probeEvent{})
	bus.RegisterSubscriber(bad.Handle, &probeEvent{})
	bus.RegisterSubscriber(ok2.Handle, &probeEvent{})

	// 第一轮：三个都跑，中间那个失败。
	err := publishAndDispatch(t, bus, pub, newProbeEvent("a"))
	if ok1.runs != 1 || bad.runs != 1 || ok2.runs != 1 {
		t.Fatalf("第一轮三个处理器都该执行，实际 %d/%d/%d", ok1.runs, bad.runs, ok2.runs)
	}

	// 失败必须以 PartialFailure 的形式上报，且只点名失败的那一个。
	var partial *mq.PartialFailure
	if !errors.As(err, &partial) {
		t.Fatalf("应当返回 PartialFailure 以便只重投失败者，实际: %T %v", err, err)
	}
	if len(partial.Scopes) != 1 {
		t.Fatalf("只有一个处理器失败，范围里却有 %d 个: %v", len(partial.Scopes), partial.Scopes)
	}
	if !strings.Contains(partial.Scopes[0], "otherRecorder") {
		t.Fatalf("范围应当指向失败的那个处理器，实际 %q", partial.Scopes[0])
	}

	// 第二轮：模拟传输层带着范围重投。这一次只有失败者该重跑。
	bad.err = nil
	retryCtx := mq.WithRetryScopes(context.Background(), partial.Scopes)
	if err := bus.Dispatch(retryCtx, pub.sent[len(pub.sent)-1]); err != nil {
		t.Fatalf("重投应当成功: %v", err)
	}
	if bad.runs != 2 {
		t.Fatalf("失败的处理器应当被重跑，实际执行 %d 次", bad.runs)
	}
	if ok1.runs != 1 || ok2.runs != 1 {
		t.Fatalf("已经成功的处理器不该被重跑，实际 %d/%d 次", ok1.runs, ok2.runs)
	}
}

func TestAllFailedHandlersAreRetriedTogether(t *testing.T) {
	// 一轮里有两个失败时，两个都要进范围。只记第一个会让另一个被永久跳过。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	bad1 := &recorder{err: errors.New("失败一")}
	good := &otherRecorder{}
	bad2 := &thirdRecorder{err: errors.New("失败二")}
	bus.RegisterSubscriber(bad1.Handle, &probeEvent{})
	bus.RegisterSubscriber(good.Handle, &probeEvent{})
	bus.RegisterSubscriber(bad2.Handle, &probeEvent{})

	err := publishAndDispatch(t, bus, pub, newProbeEvent("a"))
	var partial *mq.PartialFailure
	if !errors.As(err, &partial) {
		t.Fatalf("应当返回 PartialFailure，实际: %v", err)
	}
	if len(partial.Scopes) != 2 {
		t.Fatalf("两个处理器失败，范围里应有 2 个，实际 %v", partial.Scopes)
	}

	bad1.err, bad2.err = nil, nil
	retryCtx := mq.WithRetryScopes(context.Background(), partial.Scopes)
	if err := bus.Dispatch(retryCtx, pub.sent[len(pub.sent)-1]); err != nil {
		t.Fatalf("重投应当成功: %v", err)
	}
	if bad1.runs != 2 || bad2.runs != 2 {
		t.Fatalf("两个失败者都应被重跑，实际 %d/%d", bad1.runs, bad2.runs)
	}
	if good.runs != 1 {
		t.Fatalf("成功的处理器不该被重跑，实际 %d 次", good.runs)
	}
}

func TestUnknownScopeFallsBackToRunningEverything(t *testing.T) {
	// 处理器改了名，重投消息里的旧名字就对不上了。此时必须退回「全部执行」——
	// 宁可重复（处理器本来就必须幂等），也不能让某个处理器因为一次改名被永久跳过。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	h := &recorder{}
	bus.RegisterSubscriber(h.Handle, &probeEvent{})

	if err := bus.Publish(context.Background(), newProbeEvent("a")); err != nil {
		t.Fatal(err)
	}
	staleCtx := mq.WithRetryScopes(context.Background(), []string{"(*RenamedHandler).Handle"})
	if err := bus.Dispatch(staleCtx, pub.sent[0]); err != nil {
		t.Fatalf("分发失败: %v", err)
	}
	if h.runs != 1 {
		t.Fatalf("范围对不上时应当全部执行，实际执行 %d 次", h.runs)
	}
}

func TestPanicInOneHandlerDoesNotStopTheOthers(t *testing.T) {
	// 一个处理器的编码错误，不该变成「这条事件的所有副作用都没发生」。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	survivor := &recorder{}
	bus.RegisterSubscriber(func(context.Context, DomainEvent) error { panic("boom") }, &probeEvent{})
	bus.RegisterSubscriber(survivor.Handle, &probeEvent{})

	err := publishAndDispatch(t, bus, pub, newProbeEvent("a"))
	if survivor.runs != 1 {
		t.Fatalf("另一个处理器仍应执行，实际 %d 次", survivor.runs)
	}
	if err == nil {
		t.Fatal("panic 应当被转成错误以触发重投")
	}
}

func TestDuplicateHandlerNameIsRejected(t *testing.T) {
	// 同名处理器会让失败隔离失效：重投时两个都被匹配上，成功的那个也跟着重跑。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	h := &recorder{}
	bus.RegisterSubscriber(h.Handle, &probeEvent{})
	bus.RegisterSubscriber(h.Handle, &probeEvent{})

	if err := publishAndDispatch(t, bus, pub, newProbeEvent("a")); err != nil {
		t.Fatalf("分发失败: %v", err)
	}
	if h.runs != 1 {
		t.Fatalf("重复注册应当被忽略，实际执行 %d 次", h.runs)
	}
}

// ---------------------------------------------------------------------------
// 三种收场 —— 第一问「重来一次有没有可能成功」，第二问「不重来的话，删还是留」
//
//	能成功       -> 上抛，重投
//	不能，且是坏报文  -> ErrPoison，不重投但进死信队列（留证据）
//	不能，且是没订阅  -> nil，确认丢弃（常态，留着只是噪音）
//
// ---------------------------------------------------------------------------

func TestDispatchSendsMalformedMessageToDLQ(t *testing.T) {
	// 解不开的报文重来一万次也是同样的结果，所以不重投；但它多半来自一次
	// 不兼容的发布，原件必须留下，所以也不能确认删除。
	bus := NewAmqpBus(&capturingPublisher{}, "exchange.x", "rk", nil)
	err := bus.Dispatch(context.Background(), "{ 这不是 JSON")
	if !errors.Is(err, mq.ErrPoison) {
		t.Fatalf("无法解析的报文应当判为 ErrPoison 直接进死信，实际: %v", err)
	}
}

func TestDispatchDropsUnsubscribedEvent(t *testing.T) {
	// 全部领域事件共用一个队列，而任何一个消费进程都只关心其中一部分。
	// 「没订阅」是常态而不是异常，必须确认丢弃。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)
	bus.RegisterSubscriber(func(context.Context, DomainEvent) error { return nil }, &probeEvent{})

	if err := publishAndDispatch(t, bus, pub, newOtherEvent()); err != nil {
		t.Fatalf("未订阅的事件应当被确认丢弃，实际上抛了: %v", err)
	}
}

func TestDispatchSendsUnrestorablePayloadToDLQ(t *testing.T) {
	// 载荷与注册的类型对不上，通常正是「上线了新版事件结构而旧消息还在队列里」。
	// 重投没用，但这恰恰是最该留证据的一类——订阅了却读不懂，比外层信封坏更值得查。
	bus := NewAmqpBus(&capturingPublisher{}, "exchange.x", "rk", nil)
	bus.RegisterSubscriber(func(context.Context, DomainEvent) error {
		t.Fatal("还原失败时不该调用处理器")
		return nil
	}, &probeEvent{})

	body, err := json.Marshal(envelope{Name: "test.probe", Payload: json.RawMessage(`{"subject":123}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Dispatch(context.Background(), string(body)); !errors.Is(err, mq.ErrPoison) {
		t.Fatalf("无法还原的载荷应当判为 ErrPoison 直接进死信，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 注册期的防呆
// ---------------------------------------------------------------------------

func TestRegisterSubscriberRejectsConflictingTypeForSameName(t *testing.T) {
	// 两个不同的结构体自称同一个事件名时，消费端只能挑一个来反序列化，
	// 另一个的处理器会拿到一个字段全是零值的事件。静默接受等于埋雷。
	pub := &capturingPublisher{}
	bus := NewAmqpBus(pub, "exchange.x", "rk", nil)

	bus.RegisterSubscriber(func(context.Context, DomainEvent) error { return nil }, &probeEvent{})

	called := false
	bus.RegisterSubscriber(func(context.Context, DomainEvent) error {
		called = true
		return nil
	}, &nameCollider{})

	if err := publishAndDispatch(t, bus, pub, newProbeEvent("a")); err != nil {
		t.Fatalf("分发失败: %v", err)
	}
	if called {
		t.Fatal("类型冲突的注册应当被忽略，而不是让它收到一个类型对不上的事件")
	}
}

// nameCollider 故意与 probeEvent 同名但类型不同。
type nameCollider struct{ BaseDomainEvent }

func (e *nameCollider) Name() string { return "test.probe" }

func (e *nameCollider) ToJson() (string, error) { return "{}", nil }

func TestRegisterSubscriberIgnoresNonPointerPrototype(t *testing.T) {
	// 样例必须是指向结构体的指针，否则消费端无法 new 出实例。
	// 忽略而不是 panic：这是装配错误，不该让整个进程起不来。
	bus := NewAmqpBus(&capturingPublisher{}, "exchange.x", "rk", nil)
	bus.RegisterSubscriber(func(context.Context, DomainEvent) error { return nil }, valueEvent{})

	if len(bus.SubscribedEvents()) != 0 {
		t.Fatalf("非指针样例的注册应当被忽略，实际订阅了 %v", bus.SubscribedEvents())
	}
}

// valueEvent 用值接收者实现接口，注册时应当被拒绝。
type valueEvent struct{ BaseDomainEvent }

func (e valueEvent) Name() string { return "test.value" }

func (e valueEvent) ToJson() (string, error) { return "{}", nil }
