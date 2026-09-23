// Package amqp_handlers 把消息队列上的消息接进分析上下文。
//
// 和 http_handlers 一样，这里只做「翻译 + 转调」：解报文、转成值对象、调用 domain_service。
// 任何判断（能不能开工、失败要不要重试、重试几次）都在领域层，不在这里。
//
// 本包自己拿主意的只有一件事：**投递语义**——什么样的失败该让消息重投，
// 什么样的失败该确认丢弃。那不是业务规则，而是「消息队列保证什么」的问题，
// 因此属于 application/。
package amqp_handlers

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// Subscriber 是本处理器需要的订阅能力，由消费方声明，便于在测试里替换。
type Subscriber interface {
	SubscribeSameQueueMultipleWithContext(h mq.Handler, queueName string) error
}

// Runner 是本处理器所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *domain_services.WorkerService：本包的全部价值就在
// 「什么该重投、什么该丢弃」这几个判断上，而那正是最需要被测试覆盖的几行。
// 依赖具体服务意味着测一次重复投递要先有数据库、Redis 和一个真的 LLM。
type Runner interface {
	RunTask(ctx context.Context, taskID string) error
}

// TaskDispatchHandler 消费「这个分析任务该跑了」的消息。
//
// # 幂等
//
// 投递是至少一次的：重投、重连补投、消费者被杀都会让同一条消息到达不止一次。
// 幂等不由本包提供，也不该由本包提供——它由仓储层 ClaimTask 那条把
// status='queued' 写进 WHERE 的 UPDATE 保证，只有命中 1 行的那个副本才真正开工。
// 本包能做的只是不去破坏它：不缓存、不去重、不自作聪明地「先查一下跑没跑过」。
type TaskDispatchHandler struct {
	runner Runner
	log    *zap.Logger
}

func NewTaskDispatchHandler(runner Runner, log *zap.Logger) *TaskDispatchHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &TaskDispatchHandler{runner: runner, log: log}
}

// Register 把处理器挂到队列上。
//
// 队列名来自常量而不是字面量：它必须同时与配置里的声明、以及发布方用的路由键
// 对得上，拼错的表现是「消费者活着但永远收不到消息」——没有报错、没有日志、
// 只有一个永远不动的队列。
func (h *TaskDispatchHandler) Register(bus Subscriber) error {
	return bus.SubscribeSameQueueMultipleWithContext(h.OnReceivedMessage, constants.QueueAnalysisTaskReady)
}

// OnReceivedMessage 处理一条派发消息。
//
// 返回 nil 表示确认，返回错误表示请重投。判据只有一句：**重来一次有没有可能成功**。
//
//	报文解不开、缺字段   重来一万次也是同样的结果        -> mq.ErrPoison（进死信）
//	领域层返回错误       下游可能只是临时不可用          -> 错误（重投）
//
// 第一类若返回普通错误，消息会在重试队列和主队列之间兜圈子直到进死信，
// 除了制造噪音没有任何作用；而直接返回 nil 又会把它删掉。ErrPoison 是第三条路：
// 不重试，但留在死信队列里可查可重放。
func (h *TaskDispatchHandler) OnReceivedMessage(ctx context.Context, message string) error {
	msg, err := value_objects.NewTaskDispatchMessage(message)
	if err != nil {
		// 报文本身是坏的。这只可能来自一次不兼容的发布或者手工塞进队列的消息，
		// 重投解决不了任何问题——但正因为它指向一次发布事故，原件必须留下。
		h.log.Error("分析任务派发消息无法解析，转入死信队列",
			zap.String("message", message), zap.Error(err))
		return fmt.Errorf("分析任务派发消息无法解析: %v: %w", err, mq.ErrPoison)
	}

	h.log.Debug("收到分析任务派发消息", zap.String("task_id", msg.TaskID))
	return h.runner.RunTask(ctx, msg.TaskID)
}
