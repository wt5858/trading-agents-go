package providers

import (
	"github.com/google/wire"

	analysis_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/domain_event_handlers"
	notification_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/application/domain_event_handlers"
	notification_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/domain_services"
	notification_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/notification/repositories"
	"github.com/wt5858/trading-agents-go/internal/helpers/event_handlers"
)

// 通知是整条事件链的终点：它只消费别人的事件，自己不发事件。
//
// 各上下文与通知之间没有任何共享事务：业务方自己落库并发出事件，
// 通知独立落库。代价是最终一致（通知会比事实晚几毫秒出现），
// 换来的是任何一侧的故障都不会回滚另一侧——一次通知写入失败
// 不该让一个跑了十几分钟的分析任务作废。

var NotificationSet = wire.NewSet(
	notification_repo.NewNotificationRepository,
	notification_services.NewNotificationService,
)

// DomainEventHandlersSet 是消费端的完整订阅清单。
//
// 它把三个订阅者拉进 DomainEventHandlers 的参数表，Wire 因此必须把它们造出来——
// 否则一个谁也不依赖的订阅者会被安静地优化掉，表现为「事件发出来了但没人处理」。
var DomainEventHandlersSet = wire.NewSet(
	NotificationSet,
	ReportSet,
	ReportDomainEventSet,

	notification_events.NewNotificationSubscriber,
	analysis_events.NewBatchSettlementHandler,
	event_handlers.NewDomainEventHandlers,

	wire.Bind(new(notification_events.Notifier), new(*notification_services.NotificationService)),
)
