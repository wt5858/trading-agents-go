package value_objects

import (
	"encoding/json"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// TaskDispatchMessage 是「这个分析任务该跑了」这条消息的内容。
//
// ===========================================================================
// 为什么它只带一个 taskId
// ===========================================================================
//
// 调度上下文的 JobDueMessage 刻意不带执行记录 ID，改带「哪一次触发」
// （jobId + scheduledFor），理由是那边的重试必须新建一条审计记录，
// 而消息体是被队列原样重投的、换不了 ID。
//
// 分析任务不存在这个矛盾，因为它的重试语义相反：一个任务**自始至终只有一行**，
// 重试是把同一行从 failed 改回 queued（见 Task.Requeue），而不是新建一行。
// 于是 taskId 既是记录的标识，也是「这件待办的事」的标识，两者天然重合。
// 原样重投的消息指向的还是同一行，正是我们要的。
//
// 因此这里不需要、也不应该再塞别的字段：
//
//	attempts / batchId / 参数   都在那一行上，消费端认领时一起读出来。
//	                            放进消息就是给同一个事实制造第二个副本，
//	                            而消息会在队列里滞留——滞留期间那行改了，
//	                            两个副本就开始打架。
//
// 只带一个字段仍然定义成值对象而不是裸字符串：报文的校验要有唯一的落点，
// 「构造出来即合法」这条约定在整条消息链路上必须一致，
// 不能因为这条消息只有一个字段就在处理器里手写一次 if。
type TaskDispatchMessage struct {
	TaskID string `json:"taskId"`
}

// NewTaskDispatchMessage 解析一条派发消息。
//
// 校验在这里而不是在处理器里：值对象的职责就是「构造出来即合法」。
// 缺字段的报文重投一万次也还是缺字段，处理器拿到这个错误后应当确认并丢弃，
// 而不是让它在重试队列和主队列之间兜圈子直到进死信。
func NewTaskDispatchMessage(raw string) (TaskDispatchMessage, error) {
	var msg TaskDispatchMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return TaskDispatchMessage{}, custom_errors.Invalid("分析任务派发消息无法解析").Wrap(err)
	}
	if msg.TaskID == "" {
		return TaskDispatchMessage{}, custom_errors.Invalid("分析任务派发消息缺少 taskId")
	}
	return msg, nil
}

// ToJson 序列化，供发布方使用。
func (m TaskDispatchMessage) ToJson() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", custom_errors.Internal("分析任务派发消息序列化失败").Wrap(err)
	}
	return string(b), nil
}
