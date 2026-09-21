// Package event_handlers 存放启动期的聚合器。
//
// 它解决的是一个很具体的问题：Wire 只会构造**被需要**的东西。
// 一个谁也不依赖的订阅者，写了 provider 也不会被造出来——因为没人要它。
// 于是这里立两个结构，把「本进程要挂哪些处理器」变成一份显式的依赖清单：
// 处理器出现在构造函数的参数表里，Wire 就必须造它。
//
// 顺带的好处是「这个服务消费了什么」成了一个一眼能读完的问题，
// 而不是要靠全局搜索 Register 调用来回答。
package event_handlers

import (
	"go.uber.org/zap"

	analysis_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/domain_event_handlers"
	notification_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/application/domain_event_handlers"
	report_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/application/domain_event_handlers"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// DomainEventHandlers 汇总本进程订阅的全部领域事件处理器。
type DomainEventHandlers struct {
	broker *mq.AMQP
	bus    *domain_event.AmqpBus
	log    *zap.Logger

	notification *notification_handlers.NotificationSubscriber
	report       *report_handlers.OnTaskCompletedHandler
	batch        *analysis_handlers.BatchSettlementHandler
}

func NewDomainEventHandlers(
	broker *mq.AMQP,
	bus *domain_event.AmqpBus,
	log *zap.Logger,
	notification *notification_handlers.NotificationSubscriber,
	report *report_handlers.OnTaskCompletedHandler,
	batch *analysis_handlers.BatchSettlementHandler,
) *DomainEventHandlers {
	return &DomainEventHandlers{
		broker:       broker,
		bus:          bus,
		log:          log,
		notification: notification,
		report:       report,
		batch:        batch,
	}
}

// StartSubscribe 先把处理器注册进总线，再让总线开始消费队列。
//
// 顺序不能反。反过来的话，队列一打开就可能有消息进来，而此时注册表还是空的——
// 那些事件会被当成「本进程没订阅」确认并丢弃。这不会报错，只会让服务
// 刚启动的那几百毫秒里静静吃掉一批事件，而重启期间恰恰是事件最密集的时候。
func (d *DomainEventHandlers) StartSubscribe() error {
	d.notification.Register(d.bus)
	d.report.Register(d.bus)
	d.batch.Register(d.bus)

	if err := d.broker.SubscribeSameQueueMultipleWithContext(d.bus.Dispatch, constants.QueueDomainEvent); err != nil {
		return err
	}
	d.log.Info("领域事件订阅已就绪",
		zap.Strings("events", d.bus.SubscribedEvents()),
		zap.String("queue", constants.QueueDomainEvent))
	return nil
}
