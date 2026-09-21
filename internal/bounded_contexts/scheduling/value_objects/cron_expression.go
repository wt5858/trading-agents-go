// Package value_objects 提供定时任务上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不感知数据库、HTTP 与事务。任何需要「身份 + 状态机」的概念
// （ScheduledJob / JobExecution）都不属于这里——它们是 entities/ 的职责。
package value_objects

import (
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// maxPreviewCount 限制一次预览最多推演多少次触发。
//
// 预览是纯 CPU 推演，但 cron 的 Next 在极稀疏表达式（例如 "0 0 29 2 *"）上
// 单次要往后扫数年。不封顶的话，一个 count=100000 的请求就能把一个 HTTP 协程
// 占死几秒钟——这是典型的「参数即放大器」型资源耗尽。
const maxPreviewCount = 50

// cronParser 复用一份解析器实例。
//
// cron.ParseStandard 是无状态的纯函数，复用它没有并发问题；
// 这里显式持有只是为了让「本服务只接受 5 段标准 cron」这条约定有一个明确的落点，
// 而不是散落在多个调用点上。
var cronParser = cron.ParseStandard

// CronExpression 是校验过的 cron 表达式值对象。
//
// # 为什么不是裸 string
//
// 表达式的合法性决定了「这个任务还能不能算出下次执行时间」。如果用裸 string 传递，
// 校验就只能发生在某个调用点上，而漏掉校验的后果不是报错而是**静默失效**：
// NextRunAt 算不出来 -> 永远为零值 -> 任务再也不会被 ClaimDue 捞到。
// 把解析结果连同原文一起封进值对象，非法表达式在构造那一刻就进不了系统。
//
// # 为什么同时存 spec 与 sched
//
// spec 是对外的权威表示（落库、展示、比较都用它）；sched 是解析后的调度器，
// 只为 Next 服务。两者同生共死：sched 永远是 spec 解析出来的结果。
type CronExpression struct {
	spec  string
	sched cron.Schedule
}

// NewCronExpression 解析并校验一个 5 段标准 cron 表达式（也接受 @daily 这类描述符）。
func NewCronExpression(spec string) (CronExpression, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return CronExpression{}, custom_errors.Invalid("cron 表达式不能为空")
	}
	sched, err := cronParser(spec)
	if err != nil {
		// 不 Wrap 原始错误进 Message：robfig 的错误文案对调用方足够可读，
		// 但把它整段塞进 Message 会让接口返回值里出现库的内部措辞。
		return CronExpression{}, custom_errors.Invalid("非法的 cron 表达式 %q: %v", spec, err)
	}
	return CronExpression{spec: spec, sched: sched}, nil
}

// RehydrateCronExpression 从库里的行重建表达式，**不报错**。
//
// 数据库里的行是既成事实：某条历史记录可能是在旧的解析规则下写进去的。
// 若在读路径上再校验一次，一条陈年脏数据就能让整个任务列表接口 500。
// 解析失败时 sched 为 nil，Valid() 返回 false，Next() 返回零值——
// 这条记录在列表里照常可见、可编辑、可删除，只是不会被调度器捞起来执行。
func RehydrateCronExpression(spec string) CronExpression {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return CronExpression{}
	}
	sched, err := cronParser(spec)
	if err != nil {
		return CronExpression{spec: spec}
	}
	return CronExpression{spec: spec, sched: sched}
}

// Next 返回 after 之后的下一次触发时刻。
//
// 表达式不可用时返回零值而不是 panic：调用方（聚合）据此判定「算不出下次执行」，
// 并把它变成一个显式的领域错误，而不是一个半夜把 worker 打挂的空指针。
func (c CronExpression) Next(after time.Time) time.Time {
	if c.sched == nil {
		return time.Time{}
	}
	return c.sched.Next(after)
}

// NextN 连续推演 n 次触发时刻，供「保存前预览」使用。
//
// 推演放在值对象里而不是领域服务里：它只依赖表达式自身，不依赖任何任务状态。
// 每一步都以上一步的结果为起点，这正是 cron 的语义；用 after.Add(i*interval)
// 这种近似做法在「每月最后一天」这类表达式上会算出完全错误的序列。
func (c CronExpression) NextN(after time.Time, n int) []time.Time {
	if c.sched == nil || n <= 0 {
		return nil
	}
	if n > maxPreviewCount {
		n = maxPreviewCount
	}
	out := make([]time.Time, 0, n)
	cursor := after
	for i := 0; i < n; i++ {
		next := c.sched.Next(cursor)
		if next.IsZero() || !next.After(cursor) {
			// 表达式永远不会再触发（例如指定了一个已经过去的年份组合）。
			// 停下来而不是空转，否则这个循环就是一个活锁。
			break
		}
		out = append(out, next)
		cursor = next
	}
	return out
}

// String 返回原始表达式。落库、展示、比较都以它为准。
func (c CronExpression) String() string { return c.spec }

// Valid 表示这个表达式能否算出下次触发时刻。
func (c CronExpression) Valid() bool { return c.sched != nil }

func (c CronExpression) IsZero() bool { return c.spec == "" }

// Equal 按原文比较。值对象没有身份，相等性只由值决定；
// 不能用 == 比较结构体本身，因为 sched 是接口，两次解析出的实例并不相等。
func (c CronExpression) Equal(other CronExpression) bool { return c.spec == other.spec }
