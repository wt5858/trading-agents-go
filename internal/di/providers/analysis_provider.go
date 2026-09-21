package providers

import (
	"time"

	"github.com/google/wire"
	"github.com/redis/go-redis/v9"

	"github.com/wt5858/trading-agents-go/config"
	analysis_amqp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/application/amqp_handlers"
	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	analysis_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// 分析上下文的装配。进度快照与并发闸门这两个 Redis 仓储都需要一个 time.Duration，
// 而 Wire 不会凭空区分两个 time.Duration——所以每一个都由一个明确的 provider
// 函数从配置里取值，而不是让 Wire 去猜。

// NewTaskDispatcher 把分析任务的派发接到消息队列上。
//
// 它取代了原先基于 Redis 的 TaskQueue：可见性超时、心跳续约、崩溃回收、尝试计数
// 这四样队列层本来就提供，自己再实现一遍的代价不是代码量，是每一处都得自己保证正确。
func NewTaskDispatcher(broker *mq.AMQP) *analysis_repo.TaskDispatcher {
	return analysis_repo.NewTaskDispatcher(broker)
}

func NewProgressPublisher(rdb *redis.Client, cfg *config.Config) *analysis_repo.ProgressPublisher {
	return analysis_repo.NewProgressPublisher(rdb, cfg.Queue.ProgressTTL)
}

func NewConcurrencyGuard(rdb *redis.Client, cfg *config.Config) *analysis_repo.ConcurrencyGuard {
	return analysis_repo.NewConcurrencyGuard(rdb, cfg.Queue.SlotTTL)
}

func NewAnalysisPolicy(cfg *config.Config) analysis_services.Policy {
	return analysis_services.Policy{
		Limits: analysis_services.Limits{
			PerUser: cfg.Queue.UserConcurrency,
			Global:  cfg.Queue.GlobalConcurrency,
		},
		MaxAttempts: cfg.Queue.MaxAttempts,
	}
}

func NewAnalysisWorkerConfig(cfg *config.Config) analysis_services.WorkerConfig {
	return analysis_services.WorkerConfig{
		// 并发度不再在这里配：任务由消息队列投递，同时在跑几个由队列声明里的
		// Consumers 决定（见 constants.DefaultQueues）。这里只剩两个兜底巡检的参数。
		ReconcileInterval:  5 * time.Minute,
		ReconcileGrace:     10 * time.Minute,
		ReconcileBatchSize: 50,
		// 停滞巡检的两个宽限期。
		//
		// RunningGrace 从 AnalysisMaxRuntime 派生而不是另写一个数，理由见那个常量。
		// 多出来的一截余量是有意的，它保证两个回收机制不会互相打架：
		// broker 的 consumer_timeout 先到（消息被收回，认领失败后确认丢弃），
		// 巡检随后才介入把这行判失败并重新派发。反过来的话，巡检刚把任务改回
		// queued，那条还没超时的旧消息就可能把它再认领一次。
		QueuedGrace:  10 * time.Minute,
		RunningGrace: constants.AnalysisMaxRuntime + 5*time.Minute,
	}
}

var AnalysisSet = wire.NewSet(
	NewTaskDispatcher,
	NewProgressPublisher,
	NewConcurrencyGuard,
	NewAnalysisPolicy,
	NewAnalysisWorkerConfig,
	analysis_repo.NewTaskRepository,
	analysis_repo.NewBatchRepository,
	analysis_services.NewAnalysisService,
	analysis_services.NewBatchService,
	analysis_services.NewWorkerService,
	analysis_amqp.NewTaskDispatchHandler,
	wire.Bind(new(analysis_amqp.Runner), new(*analysis_services.WorkerService)),
)

// 注意：analysis 对 agent 的依赖（Engine）不在这里，而在 CoreSet——
// Wire 要求 wire.Bind 与具体类型的 provider 同处一个 set。详见 core_provider.go。
