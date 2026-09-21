// Package constants 集中存放跨层共享的字面量，目前只有消息队列的拓扑。
//
// 队列名必须同时出现在三个地方：配置文件里的声明、发布方的路由键、订阅方的队列名。
// 让它们各写各的字符串，迟早会有一处拼错，而拼错的表现是「消费者活着但收不到消息」——
// 没有报错、没有日志、只有一个永远不动的队列。把名字收进常量，
// 拼错就变成编译错误；配置文件与常量之间的漂移则由启动时的订阅检查兜住。
package constants

import (
	"time"

	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// Exchange 是本服务自己的交换机。
//
// 只有一个：交换机的作用是把消息按路由键分发到队列，而本服务的消息种类
// （领域事件、定时任务触发）都由自己产生、自己消费，没有任何一类需要
// 独立的权限边界或独立的生命周期。多开交换机只会让拓扑图更难看懂。
const ExchangeTradingAgents = "exchange.trading_agents"

// 领域事件走的队列与路由键。
//
// 全部领域事件共用一个队列，由分发器按事件名找处理器——而不是一个事件一个队列。
// 理由是事件的种类会随业务增长，每加一个事件就要改一次拓扑配置、建一次队列、
// 重启一次服务，这个代价会直接抑制「该发事件的地方就发事件」。
// 代价是同一个队列里的事件会互相排队，而领域事件的处理都是轻量副作用
// （写通知、生成报告），排队是可以接受的。
const (
	QueueDomainEvent      = "queue.trading_agents_event"
	RoutingKeyDomainEvent = "trading_agents_event"
)

// 定时任务到期触发走的队列与路由键。
//
// 与领域事件分开是必要的：定时任务的执行动辄几分钟（同步全市场行情、发起一次分析），
// 和它共用队列会让轻量的通知事件堵在一次行情同步后面几分钟才被处理。
// 这正是「什么时候该多开一个队列」的判据——处理时长差一个数量级。
const (
	QueueScheduledJobDue      = "queue.scheduled_job_due"
	RoutingKeyScheduledJobDue = "scheduled_job_due"
)

// AnalysisMaxRuntime 是「一次分析最长允许跑多久」，也是三个本来会各写各的
// 超时值的唯一来源。
//
// 它们量化的是同一件事，只是站在三个不同的位置上：
//
//	本常量                    领域上限：超过它就不认为任务还在合法地跑
//	broker 的 consumer_timeout 传输层上限：超过它 broker 收回未 ack 的消息
//	WorkerConfig.RunningGrace  兜底上限：超过它停滞巡检判定执行进程已死
//
// 散成三个魔数的后果不是难看，是必然有人只调其中一个：把分析调深、跑得更久之后
// 只改了 broker，巡检就会开始杀健康任务；只改了巡检，broker 会先把消息收回去。
// 两种都很难查，因为它们都表现为「任务偶尔莫名其妙失败」。
//
// 改这个值时，docker-compose.yml 里 rabbitmq 的 consumer_timeout 要一起改——
// 那一侧是 broker 的配置，没法从 Go 这边设置，只能靠两边注释互指。
const AnalysisMaxRuntime = 30 * time.Minute

// 分析任务派发走的队列与路由键。
//
// 它与定时任务分开，理由和定时任务与领域事件分开是同一条：处理时长差一个数量级。
// 一次分析是十几轮 LLM 调用、以分钟计，塞进定时任务队列会让「到点该同步行情了」
// 堵在一次分析后面。
//
// 为什么不继续用 Redis 的 list + zset：那套实现要自己维护可见性超时、心跳续约、
// 崩溃回收（一段 Lua）和尝试次数，而这四样正是本队列层已经提供的——
// 未 ack 的消息本就不可见，信道一断就自动重投，次数记在 x-retry-count 里，
// 用尽进死信。自己再实现一遍的代价不是代码量，是每一处都得自己保证正确。
const (
	QueueAnalysisTaskReady      = "queue.analysis_task_ready"
	RoutingKeyAnalysisTaskReady = "analysis_task_ready"
)

// DefaultExchanges / DefaultQueues 是配置文件没写 amqp 拓扑时使用的内置拓扑。
//
// 有这份内置拓扑，服务就能在只配了连接信息的情况下跑起来；
// 配置文件里写了则完全以配置为准（不做合并），这样运维改拓扑不必改代码，
// 而「配置里写了一半」也不会得到一个半内置半配置的、谁也说不清的结果。
func DefaultExchanges() []mq.Exchange {
	return []mq.Exchange{{
		Name:    ExchangeTradingAgents,
		Type:    "direct",
		Durable: true,
	}}
}

func DefaultQueues() []mq.Queue {
	return []mq.Queue{{
		Exchange:   ExchangeTradingAgents,
		Name:       QueueDomainEvent,
		RoutingKey: RoutingKeyDomainEvent,
		Durable:    true,
		// 领域事件的处理器都是轻量写库，多开几个消费者没有下游压力上的顾虑。
		Consumers:  4,
		Prefetch:   4,
		MaxRetries: 3,
		RetryDelay: 30 * time.Second,
	}, {
		Exchange:   ExchangeTradingAgents,
		Name:       QueueScheduledJobDue,
		RoutingKey: RoutingKeyScheduledJobDue,
		Durable:    true,
		// 并发度按下游承受能力定，不按核数定：一次触发可能是一轮全市场行情同步。
		// 预取固定为 1——预取多条只会让它们在一个消费者本地排队，旁边的副本闲着。
		Consumers: 4,
		Prefetch:  1,
		// 重投间隔比领域事件长得多：定时任务失败通常是上游整体不可用
		// （数据源限流、LLM 配额耗尽），30 秒后重来大概率还是同样的结果。
		MaxRetries: 3,
		RetryDelay: 2 * time.Minute,
	}, {
		Exchange:   ExchangeTradingAgents,
		Name:       QueueAnalysisTaskReady,
		RoutingKey: RoutingKeyAnalysisTaskReady,
		Durable:    true,
		// 消费者数就是并发分析数。预取固定为 1：一条消息是几分钟的 LLM 调用，
		// 预取多条只会让它们在一个消费者本地干等，旁边的副本闲着。
		//
		// 真正的并发上限不在这里，而在 Redis 里的并发闸门（ta:slots:global），
		// 它在提交时就扣，跨进程生效。这里多开消费者不会突破那个上限。
		Consumers: 4,
		Prefetch:  1,
		// MaxRetries 是**兜底**，不是业务重试次数，因此刻意设在领域的
		// MaxAttempts（默认 3）之上。
		//
		// 分工是这样的：任务失败时 handler 问聚合还有没有余量，有就返回错误让本队列
		// 延迟重投，没有就把任务落成终态并返回 nil（确认收场）。也就是说业务路径永远
		// 走不到这个上限——能耗尽它的只有「连任务都没读出来」这类领域判定之外的卡死，
		// 那种消息就该进死信等人看，而不是继续在队列里兜圈子。
		MaxRetries: 5,
		// 重投间隔取一分钟，而不是像旧实现那样失败后立刻重排。分析失败的常见成因是
		// LLM 配额耗尽或数据源限流，立刻重来大概率是同样的结果，只是把钱烧得更快。
		RetryDelay: time.Minute,
	}}
}
