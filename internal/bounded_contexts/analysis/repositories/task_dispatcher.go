package repositories

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MessagePublisher 是本包所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *mq.AMQP：分析上下文只需要「把消息发到某个路由键上」
// 这一件事，给它整个客户端就等于连带给了它声明拓扑、起消费者的能力。
// 这也让本仓储在测试里可以换成一个把消息收进切片的桩。
type MessagePublisher interface {
	PublishDirectMessageWithContext(ctx context.Context, exchange, routingKey, body string) error
	PublishDirectMessagesWithContext(ctx context.Context, exchange, routingKey string, bodies []string) error
}

// TaskDispatcher 把「这个分析任务该跑了」发到消息队列上。
//
// # 为什么它住在 repositories/
//
// 和 scheduling 上下文的 JobDispatcher 同理：一条躺在 broker 里等待消费的消息，
// 是一份活得比进程久、有自己生命周期的状态。判据是「这段状态活得比进程久吗」，
// 而不是「它存在关系型数据库里吗」。
//
// # 它取代了什么
//
// 它顶掉的是原先基于 Redis list + zset 的 TaskQueue。那套实现要自己维护可见性
// 超时、心跳续约、崩溃回收和尝试次数，而这四样本来就是消息队列层提供的：
// 未 ack 的消息本就不可见，信道一断自动重投，次数记在 x-retry-count 里，
// 用尽进死信。自己再实现一遍的代价不是代码量，是每一处都得自己保证正确。
type TaskDispatcher struct {
	publisher MessagePublisher
}

func NewTaskDispatcher(publisher MessagePublisher) *TaskDispatcher {
	return &TaskDispatcher{publisher: publisher}
}

// Dispatch 派发一个任务。
//
// 它只负责投递，不负责落库：任务行必须在调用本方法**之前**就已经是 queued。
// 顺序反过来的话，消费端有可能在那一行写下之前就来认领，而它什么也认领不到，
// 这个任务就这么丢了。
//
// 投递失败会如实返回错误，调用方不必自己重试——那一行还在库里 queued 着，
// 停滞巡检会把它捡起来重投。让每个调用点各写一段重试，才是真正会出错的地方。
func (d *TaskDispatcher) Dispatch(ctx context.Context, taskID string) error {
	body, err := value_objects.TaskDispatchMessage{TaskID: taskID}.ToJson()
	if err != nil {
		return err
	}
	err = d.publisher.PublishDirectMessageWithContext(
		ctx, constants.ExchangeTradingAgents, constants.RoutingKeyAnalysisTaskReady, body)
	if err != nil {
		return custom_errors.Unavailable("投递分析任务(id=%s)的派发消息失败", taskID).Wrap(err)
	}
	return nil
}

// DispatchMany 派发一批任务。
//
// 走批量发布而不是循环调 Dispatch：确认是一次真实的 broker 往返，
// 一个批次动辄几十条，串起来就是几十倍的延迟，而调用方此刻正握着数据库连接。
//
// # 部分失败是可以接受的
//
// 批量发布不是原子的，中途失败时前面几条已经发出去了。这里不做补偿，
// 因为补偿已经存在且更可靠：这批任务的行都已经是 queued，没被投递到的那些
// 会被停滞巡检捡起来重投。反过来，若在这里自作主张地回滚或重发，
// 反而会和巡检抢同一批任务。
func (d *TaskDispatcher) DispatchMany(ctx context.Context, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	bodies := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		body, err := value_objects.TaskDispatchMessage{TaskID: id}.ToJson()
		if err != nil {
			return err
		}
		bodies = append(bodies, body)
	}
	err := d.publisher.PublishDirectMessagesWithContext(
		ctx, constants.ExchangeTradingAgents, constants.RoutingKeyAnalysisTaskReady, bodies)
	if err != nil {
		return custom_errors.Unavailable("批量投递 %d 个分析任务的派发消息失败", len(taskIDs)).Wrap(err)
	}
	return nil
}
