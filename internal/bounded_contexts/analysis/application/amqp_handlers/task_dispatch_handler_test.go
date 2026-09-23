package amqp_handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// ===========================================================================
// 投递语义：什么该重投，什么该确认丢弃
// ===========================================================================
//
// 这是本包唯一自己拿主意的事，也因此是唯一值得测的事。判据只有一句：
// **重来一次有没有可能成功**。
//
// 判错的代价不对称，两个方向都很难查：
//
//	该丢的却重投   坏报文在重试队列和主队列之间兜圈子直到进死信，只制造噪音
//	该重投的却丢   任务静默消失，用户看到的是一个永远停在「排队中」的进度条
//
// 用桩而不是真的 WorkerService：这几行判断的正确性不该依赖于先有一个数据库、
// 一个 Redis 和一个真的会花钱的 LLM。

type stubRunner struct {
	called []string
	err    error
}

func (r *stubRunner) RunTask(_ context.Context, taskID string) error {
	r.called = append(r.called, taskID)
	return r.err
}

// 坏报文必须走 mq.ErrPoison：不重投，但进死信队列而不是被删掉。
//
// 这里有三种可能的收场，只有一种是对的：
//   - 返回普通错误 → 在重试队列和主队列之间兜圈子 MaxRetries 次才进死信，纯噪音；
//   - 返回 nil     → 确认并**删除**。坏报文最常见的成因是滚动发布期间新旧结构
//     不兼容，那恰恰是最需要留下原件的时刻；
//   - 返回 ErrPoison → 不重试，直接进死信队列，带着 x-dead-reason 和原始 trace_id。
func TestHandlerSendsUnparsableMessageToDLQ(t *testing.T) {
	// 两类坏报文：整个解不开的，和解得开但缺必填字段的。
	// 它们的共同点是重投一万次结果完全一样。
	for _, raw := range []string{
		`{"taskId":`,      // 断掉的 JSON
		`{}`,              // 缺 taskId
		`{"taskId":""}`,   // taskId 是空串
		`not json at all`, // 压根不是 JSON
	} {
		runner := &stubRunner{}
		h := NewTaskDispatchHandler(runner, nil)

		err := h.OnReceivedMessage(context.Background(), raw)
		if !errors.Is(err, mq.ErrPoison) {
			t.Fatalf("坏报文 %q 必须判为 ErrPoison 直接进死信，实际返回 %v", raw, err)
		}
		if len(runner.called) != 0 {
			t.Fatalf("坏报文 %q 不该触发任何执行，实际调用了 %v", raw, runner.called)
		}
	}
}

// 领域层的错误要如实往上抛：下游可能只是临时不可用，重来有可能成功。
func TestHandlerPropagatesRunnerError(t *testing.T) {
	wantErr := errors.New("数据库暂时不可用")
	runner := &stubRunner{err: wantErr}
	h := NewTaskDispatchHandler(runner, nil)

	body, err := value_objects.TaskDispatchMessage{TaskID: "task_1"}.ToJson()
	if err != nil {
		t.Fatalf("构造报文失败: %v", err)
	}

	if err := h.OnReceivedMessage(context.Background(), body); !errors.Is(err, wantErr) {
		t.Fatalf("领域层的错误必须上抛以触发重投，实际返回 %v", err)
	}
}

func TestHandlerRunsTaskOnValidMessage(t *testing.T) {
	runner := &stubRunner{}
	h := NewTaskDispatchHandler(runner, nil)

	body, err := value_objects.TaskDispatchMessage{TaskID: "task_42"}.ToJson()
	if err != nil {
		t.Fatalf("构造报文失败: %v", err)
	}

	if err := h.OnReceivedMessage(context.Background(), body); err != nil {
		t.Fatalf("正常报文不该返回错误: %v", err)
	}
	if len(runner.called) != 1 || runner.called[0] != "task_42" {
		t.Fatalf("应当以 task_42 调用一次执行，实际 %v", runner.called)
	}
}

// 报文格式是发布方与消费方之间的契约，必须经得起一次往返。
// 这条断言的价值在于：将来有人改了 json tag，这里会红，而不是等到线上
// 所有消息都变成「缺少 taskId」被静默丢弃。
func TestDispatchMessageRoundTrip(t *testing.T) {
	body, err := value_objects.TaskDispatchMessage{TaskID: "task_rt"}.ToJson()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	msg, err := value_objects.NewTaskDispatchMessage(body)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if msg.TaskID != "task_rt" {
		t.Fatalf("往返后 taskId 应为 task_rt，实际 %q", msg.TaskID)
	}
}
