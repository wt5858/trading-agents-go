package repositories

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// MessagePublisher 是本包所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *mq.AMQP：调度上下文只需要「把一个字符串发到某个路由键上」
// 这一件事，给它整个客户端就等于连带给了它声明拓扑、起消费者的能力。
// 这也让本仓储在测试里可以换成一个把消息收进切片的桩。
type MessagePublisher interface {
	PublishDirectMessageWithContext(ctx context.Context, exchange, routingKey, body string) error
}

// JobDispatcher 把「这次触发该跑了」发到消息队列上。
//
// # 为什么它住在 repositories/
//
// 因为它和 analysis 上下文里的 TaskDispatcher 是同一类东西：
// 一份存在进程之外、有自己生命周期的状态，本上下文通过它与外界交换事实。
// 「持久化」不等于「关系型数据库」——判据是「这段状态活得比进程久吗」，
// 而一条躺在 broker 里等待消费的消息显然是。
//
// 放在 domain_services/ 会让领域服务直接认识消息中间件；放在 di/ 则会让
// 调度上下文的一块核心机制散落到组装根里，读这个上下文的人得先读组装根才能理解它。
type JobDispatcher struct {
	publisher MessagePublisher
}

func NewJobDispatcher(publisher MessagePublisher) *JobDispatcher {
	return &JobDispatcher{publisher: publisher}
}

// Dispatch 发出一条到期消息。
//
// 它只负责投递，不负责记录：欠条（那条 queued 执行记录）必须在调用本方法**之前**
// 就已经落库。顺序反过来的话，消费端有可能在记录写下之前就已经来认领了，
// 而它会认领不到任何东西，于是这次触发就这么丢了。
//
// 投递失败会如实返回错误。调用方不需要为此重试——那条 queued 记录还在库里欠着，
// 恢复巡检会把它捡起来重投。让每个调用点各写一段重试，才是真正会出错的地方。
func (d *JobDispatcher) Dispatch(ctx context.Context, msg value_objects.JobDueMessage) error {
	body, err := msg.ToJson()
	if err != nil {
		return err
	}
	err = d.publisher.PublishDirectMessageWithContext(
		ctx, constants.ExchangeTradingAgents, constants.RoutingKeyScheduledJobDue, body)
	if err != nil {
		return custom_errors.Unavailable("投递定时任务(id=%s)的到期消息失败", msg.JobID).Wrap(err)
	}
	return nil
}
