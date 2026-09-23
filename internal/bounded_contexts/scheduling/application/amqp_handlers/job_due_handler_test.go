package amqp_handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// 本包的全部价值就在一个判断上：**什么样的失败该让消息重投，什么样的该确认丢弃**。
// 判错的代价是具体的——该丢的不丢，消息在重试队列和主队列之间兜圈子直到进死信；
// 该重投的不重投，一次定时触发静默消失。所以这里测的就是这一个判断。

type fakeExecutor struct {
	calls []value_objects.JobDueMessage
	err   error
}

func (f *fakeExecutor) ExecuteQueued(_ context.Context, msg value_objects.JobDueMessage) error {
	f.calls = append(f.calls, msg)
	return f.err
}

func validMessage(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(value_objects.JobDueMessage{
		JobID:        "job_1",
		ScheduledFor: time.Now().Truncate(time.Millisecond),
		Manual:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestForwardsValidMessageToDomain(t *testing.T) {
	exec := &fakeExecutor{}
	h := NewJobDueHandler(exec, nil)

	if err := h.OnReceivedMessage(context.Background(), validMessage(t)); err != nil {
		t.Fatalf("正常消息不该报错: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("应当转调领域层一次，实际 %d 次", len(exec.calls))
	}
	if exec.calls[0].JobID != "job_1" {
		t.Fatalf("任务 ID 没有正确传递: %+v", exec.calls[0])
	}
}

func TestPropagatesDomainErrorToTriggerRedelivery(t *testing.T) {
	// 领域层的错误意味着「下游可能只是临时不可用」，重来有可能成功。
	// 吞掉它等于把一次本可以自动恢复的失败变成永久失败。
	sentinel := errors.New("上游 500")
	h := NewJobDueHandler(&fakeExecutor{err: sentinel}, nil)

	err := h.OnReceivedMessage(context.Background(), validMessage(t))
	if !errors.Is(err, sentinel) {
		t.Fatalf("领域层的错误应当原样上抛以触发重投，实际: %v", err)
	}
}

// 坏报文走 mq.ErrPoison：不重投，但进死信队列而不是被删掉。
//
// 上抛普通错误只会让它在重试队列和主队列之间兜圈子直到进死信，除了噪音没有作用；
// 返回 nil 则会把它删掉——而坏报文最常见的成因是滚动发布期间新旧结构不兼容，
// 那恰恰是最需要留下原件的时刻。ErrPoison 两头都避开。
func TestSendsUnparseableMessageToDLQ(t *testing.T) {
	cases := map[string]string{
		"不是 JSON":        "{ 这不是 JSON",
		"缺 jobId":        `{"scheduledFor":"2026-09-17T10:00:00Z"}`,
		"缺 scheduledFor": `{"jobId":"job_1"}`,
		"空对象":            `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExecutor{}
			h := NewJobDueHandler(exec, nil)

			err := h.OnReceivedMessage(context.Background(), body)
			if !errors.Is(err, mq.ErrPoison) {
				t.Fatalf("无法解析的报文应当判为 ErrPoison 直接进死信，实际: %v", err)
			}
			if len(exec.calls) != 0 {
				t.Fatal("坏报文不该被转调到领域层")
			}
		})
	}
}
