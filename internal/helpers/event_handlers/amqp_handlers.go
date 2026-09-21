package event_handlers

import (
	"go.uber.org/zap"

	analysis_amqp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/amqp_handlers"
	scheduling_amqp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/application/amqp_handlers"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// AmqpHandlers 汇总本进程消费的全部队列处理器。
//
// # 为什么订阅不写在各个处理器的构造函数里
//
// 团队既有服务的做法是让处理器在自己的构造函数里直接 Subscribe，构造即上线。
// 这里刻意没有跟：本服务的 HTTP 进程与 worker 进程共用同一套装配，
// 而 HTTP 进程**必须**能把处理器造出来（它要复用同一批领域服务）却
// **绝不能**消费任何队列——两个进程都消费的话，一条消息会随机落到其中一个。
//
// 构造与上线因此必须是两步：Wire 负责构造，Start 负责上线。
type AmqpHandlers struct {
	broker *mq.AMQP
	log    *zap.Logger

	jobDue       *scheduling_amqp.JobDueHandler
	taskDispatch *analysis_amqp.TaskDispatchHandler
}

func NewAmqpHandlers(
	broker *mq.AMQP,
	log *zap.Logger,
	jobDue *scheduling_amqp.JobDueHandler,
	taskDispatch *analysis_amqp.TaskDispatchHandler,
) *AmqpHandlers {
	return &AmqpHandlers{broker: broker, log: log, jobDue: jobDue, taskDispatch: taskDispatch}
}

// Start 把全部处理器挂到各自的队列上。
func (h *AmqpHandlers) Start() error {
	if err := h.jobDue.Register(h.broker); err != nil {
		return err
	}
	if err := h.taskDispatch.Register(h.broker); err != nil {
		return err
	}
	h.log.Info("消息队列处理器已就绪")
	return nil
}
