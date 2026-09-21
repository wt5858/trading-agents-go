package mq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件只覆盖不需要 broker 的那部分：连接串拼装、重试计数解析、
// 队列默认值与 panic 兜底。需要真实 broker 的部分（拓扑声明、投递、重投路由）
// 属于集成测试，不在单元测试里假装覆盖。

func TestConnectionInfoURL(t *testing.T) {
	cases := []struct {
		name string
		in   ConnectionInfo
		want string
	}{{
		name: "默认 vhost 不应该在路径里留下斜杠",
		in:   ConnectionInfo{Host: "rabbit", Port: 5672, VHost: "/", User: "u", Password: "p"},
		want: "amqp://u:p@rabbit:5672/",
	}, {
		name: "具名 vhost",
		in:   ConnectionInfo{Host: "rabbit", Port: 5672, VHost: "/trading", User: "u", Password: "p"},
		want: "amqp://u:p@rabbit:5672/trading",
	}, {
		name: "密码里的特殊字符必须转义，否则会被当成 URL 分隔符",
		in:   ConnectionInfo{Host: "rabbit", Port: 5672, VHost: "/", User: "u", Password: "p@ss/word"},
		want: "amqp://u:p%40ss%2Fword@rabbit:5672/",
	}, {
		name: "零值补默认",
		in:   ConnectionInfo{},
		want: "amqp://guest:guest@127.0.0.1:5672/",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.URL(); got != c.want {
				t.Fatalf("URL() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSafeURLHidesPassword(t *testing.T) {
	info := ConnectionInfo{Host: "rabbit", Port: 5672, VHost: "/", User: "u", Password: "s3cret"}
	got := info.SafeURL()
	if strings.Contains(got, "s3cret") {
		t.Fatalf("SafeURL() 泄露了密码: %s", got)
	}
	if !strings.Contains(got, "u") || !strings.Contains(got, "rabbit:5672") {
		t.Fatalf("SafeURL() 应当保留足以定位 broker 的信息，实际: %s", got)
	}
}

func TestRetryCountOfAcceptsEveryIntegerWidth(t *testing.T) {
	// AMQP 的 Table 在网络上是弱类型的：同一个整数可能以任意宽度回来，
	// 取决于发送方用的是哪个客户端。认漏一种就意味着重试计数被重置成 0，
	// 消息会一直重投下去而永远进不了死信队列。
	cases := map[string]struct {
		headers map[string]any
		want    int
	}{
		"缺失":      {map[string]any{}, 0},
		"int32":   {map[string]any{headerRetryCount: int32(3)}, 3},
		"int64":   {map[string]any{headerRetryCount: int64(3)}, 3},
		"int":     {map[string]any{headerRetryCount: 3}, 3},
		"float64": {map[string]any{headerRetryCount: float64(3)}, 3},
		"不认识的类型":  {map[string]any{headerRetryCount: "3"}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := retryCountOf(c.headers); got != c.want {
				t.Fatalf("retryCountOf() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestQueueNormalizedFillsDefaults(t *testing.T) {
	def := Options{MaxRetries: 5, RetryDelay: 30 * time.Second}.normalized()

	got := Queue{Name: "queue.x"}.normalized(def)
	if got.Prefetch != 1 {
		t.Fatalf("Prefetch 默认应为 1，实际 %d", got.Prefetch)
	}
	if got.Consumers != 1 {
		t.Fatalf("Consumers 默认应为 1，实际 %d", got.Consumers)
	}
	if got.MaxRetries != def.MaxRetries || got.RetryDelay != def.RetryDelay {
		t.Fatalf("未配置的重试参数应当回落到客户端默认值，实际 %d/%s", got.MaxRetries, got.RetryDelay)
	}

	// 队列自己配了就不该被默认值覆盖。
	custom := Queue{Name: "queue.y", MaxRetries: 2, RetryDelay: time.Second}.normalized(def)
	if custom.MaxRetries != 2 || custom.RetryDelay != time.Second {
		t.Fatalf("队列自身的重试配置被默认值覆盖了: %d/%s", custom.MaxRetries, custom.RetryDelay)
	}
}

func TestRetryAndDLQNamesAreDerived(t *testing.T) {
	q := Queue{Name: "queue.job_due"}
	if q.RetryName() != "queue.job_due.retry" {
		t.Fatalf("RetryName() = %s", q.RetryName())
	}
	if q.DLQName() != "queue.job_due.dlq" {
		t.Fatalf("DLQName() = %s", q.DLQName())
	}
}

func TestInvokeConvertsPanicToError(t *testing.T) {
	// 处理器 panic 必须变成一个普通的处理失败，走和其他失败一样的重试路径。
	// 让它逃逸出去会带走整个消费循环，一条坏消息就能让整条队列停摆。
	err := invoke(context.Background(), func(context.Context, string) error {
		panic("boom")
	}, "{}")
	if err == nil {
		t.Fatal("panic 应当被转换成错误")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误信息应当保留 panic 的内容，实际: %v", err)
	}
}

func TestInvokePassesThroughHandlerError(t *testing.T) {
	sentinel := errors.New("handler failed")
	if err := invoke(context.Background(), func(context.Context, string) error {
		return sentinel
	}, "{}"); !errors.Is(err, sentinel) {
		t.Fatalf("处理器的错误应当原样返回，实际: %v", err)
	}
}

func TestTruncateReasonKeepsUTF8Intact(t *testing.T) {
	long := strings.Repeat("失败原因", 500) // 远超上限，且每个字符 3 字节
	got := truncateReason(long)
	if len(got) > maxReasonLen {
		t.Fatalf("截断后长度 %d 超过上限 %d", len(got), maxReasonLen)
	}
	// 截断点落在多字节字符中间会产生乱码，消息头里出现乱码会让排查更困难。
	for _, r := range got {
		if r == '�' {
			t.Fatal("截断产生了非法的 UTF-8 序列")
		}
	}
}

func TestSubscribeRejectsUndeclaredQueue(t *testing.T) {
	// 订阅一个没声明的队列，若默默建一个默认参数的队列，拼错的队列名就会变成
	// 「消费者活着但永远收不到消息」的幽灵——那是最难发现的一类故障。
	a := New(ConnectionInfo{}, Options{})
	defer func() { _ = a.Close() }()

	err := a.SubscribeSameQueueMultipleWithContext(
		func(context.Context, string) error { return nil }, "queue.typo")
	if err == nil {
		t.Fatal("订阅未声明的队列应当报错")
	}
	if !strings.Contains(err.Error(), "queue.typo") {
		t.Fatalf("错误信息应当指出是哪个队列，实际: %v", err)
	}
}
