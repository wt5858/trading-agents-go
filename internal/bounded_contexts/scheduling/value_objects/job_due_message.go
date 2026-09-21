package value_objects

import (
	"encoding/json"
	"time"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// JobDueMessage 是「这一次触发该跑了」这条消息的内容。
//
// ===========================================================================
// 为什么它只带「哪一次触发」，不带执行记录 ID
// ===========================================================================
//
// 最自然的写法是让消息带上执行记录 ID：调度器建一条记录、把 ID 发出去、
// 消费端按 ID 取出来跑。这个写法在**重试**上会直接撞墙。
//
// 一次失败的尝试必须留下一条 failed 的审计记录（否则重试三次的任务在历史里
// 只看得到最后一次，前两次为什么失败无从查起）。而审计记录一旦落成终态就不可改写。
// 于是重试必须是**新的一条记录**——但消息体是由消息队列原样重投的，
// 它没有办法在重投时换一个 ID。带上 ID 的消息，重投回来必然指向一条已经终结的记录。
//
// 改成只带「哪一次触发」（jobId + scheduledFor）之后，这个矛盾消失了：
// 消息标识的是**一件待办的事**，而不是**一条具体的记录**。消费端每次收到消息，
// 都去认领这次触发当前欠着的那条 queued 记录；重试时调度上下文先补一条新的 queued 记录，
// 重投回来的同一条消息自然就认领到了新记录。
//
// ===========================================================================
// scheduledFor 为什么是标识的一部分
// ===========================================================================
//
// 只带 jobId 的话，一条每分钟执行的任务在队列里积压时，后到的消息分不清
// 自己代表的是哪一次触发，也就无法判断「这次是不是已经被跑过了」。
// (jobId, scheduledFor) 才是一次触发的完整身份，去重与幂等都建立在它上面。
type JobDueMessage struct {
	JobID string `json:"jobId"`
	// ScheduledFor 是这次触发的计划时刻，由聚合的 ClaimOccurrence 定下。
	ScheduledFor time.Time `json:"scheduledFor"`
	// Manual 区分自动 sweep 与管理员手动触发，只影响审计与统计口径，不影响执行本身。
	Manual bool `json:"manual"`
}

// NewJobDueMessage 解析一条到期消息。
//
// 校验在这里而不是在处理器里：值对象的职责就是「构造出来即合法」。
// 缺字段的报文重投一万次也还是缺字段，处理器拿到这个错误后应当确认并丢弃，
// 而不是让它在重试队列里兜圈子。
func NewJobDueMessage(raw string) (JobDueMessage, error) {
	var msg JobDueMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return JobDueMessage{}, custom_errors.Invalid("到期任务消息无法解析").Wrap(err)
	}
	if msg.JobID == "" {
		return JobDueMessage{}, custom_errors.Invalid("到期任务消息缺少 jobId")
	}
	if msg.ScheduledFor.IsZero() {
		return JobDueMessage{}, custom_errors.Invalid("到期任务消息缺少 scheduledFor")
	}
	// 与执行记录落库时的精度对齐（datetime(3)）。scheduledFor 是消费端认领执行时
	// WHERE 里的等值条件，差一个纳秒就什么都查不到——而那表现为
	// 「消息收到了，但没有任何记录被认领」，一次触发静默消失。
	msg.ScheduledFor = msg.ScheduledFor.Truncate(time.Millisecond)
	return msg, nil
}

// ToJson 序列化，供发布方使用。
func (m JobDueMessage) ToJson() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", custom_errors.Internal("到期任务消息序列化失败").Wrap(err)
	}
	return string(b), nil
}
