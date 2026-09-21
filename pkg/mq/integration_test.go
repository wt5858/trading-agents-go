package mq

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// 本文件是**集成测试**：它需要一个真实的 broker。
//
// 为什么非要真机不可：这个包里最容易写错的几件事——拓扑声明的参数、
// 发布确认的等待、重试队列的死信回流、消息头在转投时有没有被保住——
// 全都发生在与 broker 的交互里。用假对象测这些，测的是我对 AMQP 的理解，
// 而不是 AMQP 的实际行为，而恰恰是前者才会出错。
//
// 没有 broker 时整组跳过，而不是失败：单元测试必须在一台干净的机器上能跑。
//
//	docker run -d --rm -p 5672:5672 rabbitmq:3.13-alpine

func brokerInfo() ConnectionInfo {
	return ConnectionInfo{Host: "127.0.0.1", Port: 5672, VHost: "/", User: "guest", Password: "guest"}
}

// requireBroker 在 broker 不可达时跳过整个用例。
func requireBroker(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:5672", 500*time.Millisecond)
	if err != nil {
		t.Skip("没有可用的 RabbitMQ，跳过集成测试")
	}
	_ = conn.Close()
}

// newTestClient 建一个带独立拓扑的客户端。
//
// 每个用例用自己的队列名（由 name 区分），否则并行跑的用例会互相偷对方的消息——
// 而那种失败是间歇性的，比直接失败难查得多。
func newTestClient(t *testing.T, name string, q Queue) (*AMQP, Queue) {
	t.Helper()

	exchange := "test.exchange." + name
	q.Exchange = exchange
	q.Name = "test.queue." + name
	q.RoutingKey = "rk." + name
	q.Durable = false

	client := New(brokerInfo(), Options{ReconnectDelay: 200 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })

	err := client.InitiateSubscriber(
		[]Exchange{{Name: exchange, Type: "direct", Durable: false}},
		[]Queue{q},
	)
	if err != nil {
		t.Fatalf("声明拓扑失败: %v", err)
	}
	return client, q
}

// TestPublishAndConsume 走通最基本的一条：发出去，收回来，内容不变。
func TestPublishAndConsume(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "roundtrip", Queue{})

	received := make(chan string, 1)
	if err := client.SubscribeSameQueueMultipleWithContext(func(_ context.Context, msg string) error {
		received <- msg
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	body := `{"hello":"世界"}`
	if err := client.PublishDirectMessageWithContext(context.Background(), q.Exchange, q.RoutingKey, body); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case got := <-received:
		if got != body {
			t.Fatalf("收到的内容与发出的不一致：%q vs %q", got, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没有收到消息")
	}
}

// TestPublishToMissingExchangeFails 守住发布确认真的在起作用。
//
// 没有确认的话，Publish 只代表「写进了本地 socket 缓冲」，发往一个不存在的
// 交换机会**静默成功**——而本服务用消息驱动定时任务，「以为发出去了其实没有」
// 等于一次触发被吞掉，且不留任何痕迹。
func TestPublishToMissingExchangeFails(t *testing.T) {
	requireBroker(t)
	client, _ := newTestClient(t, "missing_exchange", Queue{})

	err := client.PublishDirectMessageWithContext(
		context.Background(), "exchange.does.not.exist", "rk", `{}`)
	if err == nil {
		t.Fatal("发往不存在的交换机必须报错，静默成功会让消息凭空消失")
	}
}

// TestFailedMessageIsRetriedThenDeadLettered 是本包最要紧的一条路径。
//
// 它同时验证三件事：
//   - 失败的消息确实会回来（重试队列 -> 死信路由 -> 主队列 这条回流是通的）
//   - 重投次数受上限约束，不会无限打转
//   - 次数用尽后消息进入死信队列，而不是被丢弃或卡在主队列里
func TestFailedMessageIsRetriedThenDeadLettered(t *testing.T) {
	requireBroker(t)

	const maxRetries = 2
	client, q := newTestClient(t, "retry", Queue{
		MaxRetries: maxRetries,
		// 间隔压到最小，否则这个用例要跑几十秒。
		RetryDelay: 200 * time.Millisecond,
	})

	var mu sync.Mutex
	attempts := 0
	done := make(chan struct{})

	if err := client.SubscribeSameQueueMultipleWithContext(func(context.Context, string) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == maxRetries+1 {
			// 这是最后一次投递，之后应当进死信队列。
			close(done)
		}
		return errors.New("每次都失败")
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	if err := client.PublishDirectMessageWithContext(
		context.Background(), q.Exchange, q.RoutingKey, `{"n":1}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		mu.Lock()
		got := attempts
		mu.Unlock()
		t.Fatalf("消息没有按预期重投：期望投递 %d 次，实际 %d 次", maxRetries+1, got)
	}

	// 再等一会儿，确认它**不会**继续重投——上限必须是真的上限。
	time.Sleep(2 * time.Second)
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != maxRetries+1 {
		t.Fatalf("重投次数超出上限：期望 %d 次投递，实际 %d 次", maxRetries+1, got)
	}

	// 「不再重投」有两种可能：进了死信队列，或者被悄悄丢了。
	// 后者是灾难性的——一条消息消失得无声无息——所以必须确认它真的躺在死信队列里。
	dead := fetchOne(t, q.DLQName())
	if dead == nil {
		t.Fatal("次数用尽的消息没有进入死信队列，它被丢掉了")
	}
	if string(dead.Body) != `{"n":1}` {
		t.Fatalf("死信消息的内容被改动了: %s", dead.Body)
	}
	// 死信队列里躺着一条没有任何上下文的报文，是排查时最无从下手的情况。
	if reason, ok := dead.Headers[headerDeadReason]; !ok || reason == "" {
		t.Fatal("死信消息必须带上失败原因，否则没人知道它为什么在这里")
	}
	if n := retryCountOf(dead.Headers); n != maxRetries {
		t.Fatalf("死信消息上的重投次数应为 %d，实际 %d", maxRetries, n)
	}
}

// fetchOne 从队列里取一条消息，取不到返回 nil。
// 直接开一条裸连接而不是走本包的订阅：死信队列没有消费者是设计的一部分，
// 给它挂一个消费者会把「消息在里面躺着」这件事本身给破坏掉。
func fetchOne(t *testing.T, queue string) *amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(brokerInfo().URL())
	if err != nil {
		t.Fatalf("连接 broker 失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("打开信道失败: %v", err)
	}
	defer func() { _ = ch.Close() }()

	// 消息进死信队列是异步的，给它一点时间。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("读取队列 %s 失败: %v", queue, err)
		}
		if ok {
			return &msg
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// TestSucceededMessageIsNotRedelivered 守住确认真的生效了。
// 确认没生效的表现是每条消息被处理无数遍，而处理器大多是幂等的，
// 于是这个故障可以在生产上安静地跑很久，只表现为「下游调用量莫名其妙很高」。
func TestSucceededMessageIsNotRedelivered(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "ack", Queue{RetryDelay: 200 * time.Millisecond})

	var mu sync.Mutex
	count := 0
	if err := client.SubscribeSameQueueMultipleWithContext(func(context.Context, string) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	if err := client.PublishDirectMessageWithContext(
		context.Background(), q.Exchange, q.RoutingKey, `{}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	time.Sleep(2 * time.Second)
	mu.Lock()
	got := count
	mu.Unlock()
	if got != 1 {
		t.Fatalf("成功处理的消息应当只被投递 1 次，实际 %d 次", got)
	}
}

// TestPanicInHandlerDoesNotKillConsumer 守住一条坏消息不会让整条队列停摆。
func TestPanicInHandlerDoesNotKillConsumer(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "panic", Queue{MaxRetries: 1, RetryDelay: 200 * time.Millisecond})

	survived := make(chan string, 1)
	if err := client.SubscribeSameQueueMultipleWithContext(func(_ context.Context, msg string) error {
		if msg == `{"bad":true}` {
			panic("处理器炸了")
		}
		survived <- msg
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	ctx := context.Background()
	if err := client.PublishDirectMessageWithContext(ctx, q.Exchange, q.RoutingKey, `{"bad":true}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if err := client.PublishDirectMessageWithContext(ctx, q.Exchange, q.RoutingKey, `{"good":true}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case got := <-survived:
		if got != `{"good":true}` {
			t.Fatalf("收到了意料之外的消息: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("一条 panic 的消息让消费者停止了工作")
	}
}

// TestJsonPublishRoundTrip 确认 JSON 便捷方法与字符串方法产出一致的报文。
func TestJsonPublishRoundTrip(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "json", Queue{})

	received := make(chan string, 1)
	if err := client.SubscribeSameQueueMultipleWithContext(func(_ context.Context, msg string) error {
		received <- msg
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	payload := map[string]any{"jobId": "job_1", "attempt": 2}
	if err := client.PublishDirectMessageJsonWithContext(
		context.Background(), q.Exchange, q.RoutingKey, payload); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case got := <-received:
		want := `{"attempt":2,"jobId":"job_1"}`
		if got != want {
			t.Fatalf("报文不符：%s，want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没有收到消息")
	}
}

// TestTopologyDeclarationIsIdempotent 确认重复声明同一套拓扑不会报错。
// 这一点在重连路径上是刚需：每次重连都会重新声明一遍。
func TestTopologyDeclarationIsIdempotent(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "idempotent_topology", Queue{})

	for i := 0; i < 3; i++ {
		err := client.InitiateSubscriber(
			[]Exchange{{Name: q.Exchange, Type: "direct", Durable: false}},
			[]Queue{q},
		)
		if err != nil {
			t.Fatalf("第 %d 次声明拓扑失败: %v", i+1, err)
		}
	}
}

// TestRetryScopeSurvivesRoundTrip 走真实 broker 验证重跑范围确实能穿过一次重投。
//
// 单元测试只能证明「总线报了范围、也会按范围跳过」，证明不了范围真的写进了
// 消息头、经过重试队列的死信回流之后还在。而那段路正是最容易丢东西的地方。
func TestRetryScopeSurvivesRoundTrip(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "retry_scope", Queue{
		MaxRetries: 3,
		RetryDelay: 200 * time.Millisecond,
	})

	var mu sync.Mutex
	var seen [][]string
	done := make(chan struct{})

	if err := client.SubscribeSameQueueMultipleWithContext(func(ctx context.Context, _ string) error {
		mu.Lock()
		seen = append(seen, RetryScopesFrom(ctx))
		n := len(seen)
		mu.Unlock()

		if n == 1 {
			// 第一轮：报告只有 handler-b 失败。
			return &PartialFailure{Scopes: []string{"handler-b"}, Err: errors.New("b 失败")}
		}
		close(done)
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	if err := client.PublishDirectMessageWithContext(
		context.Background(), q.Exchange, q.RoutingKey, `{}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("重投没有回来")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("应当至少投递两次，实际 %d 次", len(seen))
	}
	if len(seen[0]) != 0 {
		t.Fatalf("首次投递不该带范围（那意味着全部执行），实际 %v", seen[0])
	}
	if len(seen[1]) != 1 || seen[1][0] != "handler-b" {
		t.Fatalf("重投应当带回上一轮报告的范围，实际 %v", seen[1])
	}
}

// TestPlainErrorClearsRetryScope 守住一个容易忽略的方向：
// 一个没有给出范围的失败意味着「整条都得重来」，此时必须**清掉**旧范围。
// 留着上一轮的范围，会让真正该重跑的那个处理器被永久跳过。
func TestPlainErrorClearsRetryScope(t *testing.T) {
	requireBroker(t)
	client, q := newTestClient(t, "scope_cleared", Queue{
		MaxRetries: 3,
		RetryDelay: 200 * time.Millisecond,
	})

	var mu sync.Mutex
	var seen [][]string
	done := make(chan struct{})

	if err := client.SubscribeSameQueueMultipleWithContext(func(ctx context.Context, _ string) error {
		mu.Lock()
		seen = append(seen, RetryScopesFrom(ctx))
		n := len(seen)
		mu.Unlock()

		switch n {
		case 1:
			return &PartialFailure{Scopes: []string{"handler-b"}, Err: errors.New("b 失败")}
		case 2:
			// 这一轮返回普通错误：语义是「整条重来」。
			return errors.New("整体失败")
		}
		close(done)
		return nil
	}, q.Name); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	if err := client.PublishDirectMessageWithContext(
		context.Background(), q.Exchange, q.RoutingKey, `{}`); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("重投没有回来")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 3 {
		t.Fatalf("应当至少投递三次，实际 %d 次", len(seen))
	}
	if len(seen[2]) != 0 {
		t.Fatalf("普通错误之后的重投必须回到「全部执行」，实际带着范围 %v", seen[2])
	}
}
