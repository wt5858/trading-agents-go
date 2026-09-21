package providers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/wire"
	"go.uber.org/zap"

	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	scheduling_amqp "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/application/amqp_handlers"
	scheduling_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_services"
	scheduling_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories"
	scheduling_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 调度上下文的装配，以及它与业务上下文之间的那两个接缝（JobRunner）。
//
// 调度上下文只认 JobRunner 这个签名，完全不知道行情同步和分析任务的存在；
// 被调度的上下文也不知道自己被调度。两边的接缝都收在这里，
// 「谁依赖谁」因此是一份可以一眼读完的清单，而不是散落在各处的 import。

func NewSchedulerConfig() scheduling_services.SchedulerConfig {
	return scheduling_services.SchedulerConfig{
		ClaimLimit:  50,
		Parallelism: 4,
		// 三次尝试覆盖绝大多数临时故障（限流、超时、下游重启）。
		// 再多就不是「临时」了，该交给熔断而不是继续重试。
		MaxAttempts: 3,
		// 必须大于队列配置的重投间隔（2m），否则恢复巡检会去补投一条
		// 其实正躺在重试队列里等着的消息。
		QueuedGrace: 5 * time.Minute,
		// 必须显著大于任务自身的执行超时：一次全市场行情同步跑十几分钟是正常的。
		RunningGrace:    30 * time.Minute,
		RecoverInterval: time.Minute,
	}
}

// MarketSyncRunner 把行情同步接到调度器上。
type MarketSyncRunner struct {
	sync *stock_services.SyncService
}

var _ scheduling_services.JobRunner = (*MarketSyncRunner)(nil)

func NewMarketSyncRunner(sync *stock_services.SyncService) *MarketSyncRunner {
	return &MarketSyncRunner{sync: sync}
}

func (r *MarketSyncRunner) Kind() scheduling_vo.JobKind { return scheduling_vo.JobKindMarketSync }

// Run 从 payload 里取同步类型与市场。
//
// 参数校验放在这里而不是让 SyncService 容忍缺省值：定时任务的 payload 是人写的 JSON，
// 少写一个字段就该在执行时明确报错，而不是默默同步了别的东西。
func (r *MarketSyncRunner) Run(ctx context.Context, payload scheduling_vo.JobPayload) (string, int, error) {
	kind := payload.Str("kind")
	market := payload.Str("market")
	if kind == "" || market == "" {
		return "", 0, custom_errors.Invalid("market_sync 任务的 payload 必须包含 kind 与 market")
	}
	return r.sync.RunSync(ctx, kind, market, "scheduler")
}

// ScheduledAnalysisRunner 把「定时分析」接到调度器上。
//
// 它只负责把任务排进分析队列，不等待分析完成：一次分析要跑几分钟到十几分钟，
// 占住一个消息消费者名额那么久，会让同一个队列上的其他定时任务全部排在它后面，
// 而分析上下文本来就有自己的队列与并发配额。
type ScheduledAnalysisRunner struct {
	analysis *analysis_services.AnalysisService
}

var _ scheduling_services.JobRunner = (*ScheduledAnalysisRunner)(nil)

func NewScheduledAnalysisRunner(analysis *analysis_services.AnalysisService) *ScheduledAnalysisRunner {
	return &ScheduledAnalysisRunner{analysis: analysis}
}

func (r *ScheduledAnalysisRunner) Kind() scheduling_vo.JobKind {
	return scheduling_vo.JobKindScheduledAnalysis
}

func (r *ScheduledAnalysisRunner) Run(ctx context.Context, payload scheduling_vo.JobPayload) (string, int, error) {
	code := payload.Str("code")
	if code == "" {
		return "", 0, custom_errors.Invalid("scheduled_analysis 任务的 payload 必须包含 code")
	}
	userID, ok := payload.Int("userId")
	if !ok || userID <= 0 {
		// 定时分析要归属到某个用户：任务、配额、报告都是按用户隔离的，
		// 没有归属就无处安放产出。
		return "", 0, custom_errors.Invalid("scheduled_analysis 任务的 payload 必须包含 userId")
	}
	depth, _ := payload.Int("depth")

	op := analysis_services.Operator{UserID: uint64(userID)}
	task, err := r.analysis.Submit(ctx, op, analysis_services.SubmitInput{
		Code:     code,
		Market:   payload.Str("market"),
		Depth:    int(depth),
		LLMModel: payload.Str("llmModel"),
	})
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("已提交 %s 的分析任务 %s", code, task.ID), 1, nil
}

// NewSchedulerService 建调度服务并注册运行器。
//
// 注册放在 provider 里而不是交给 Wire：Register 是一个**副作用**
// （往 map 里塞东西），不是一次构造。Wire 只负责「谁需要什么」，
// 「构造完还要做点什么」这种事必须由人写出来，否则它会散落在
// 各个调用点，而漏掉一处的表现是某一类任务永远被记成 skipped。
func NewSchedulerService(
	jobs *scheduling_repo.ScheduledJobRepository,
	executions *scheduling_repo.JobExecutionRepository,
	dispatcher *scheduling_repo.JobDispatcher,
	publisher domain_event.Publisher,
	log *zap.Logger,
	cfg scheduling_services.SchedulerConfig,
	marketSync *MarketSyncRunner,
	scheduledAnalysis *ScheduledAnalysisRunner,
) *scheduling_services.SchedulerService {
	svc := scheduling_services.NewSchedulerService(jobs, executions, dispatcher, publisher, log, cfg)
	svc.Register(marketSync)
	svc.Register(scheduledAnalysis)
	return svc
}

var SchedulingSet = wire.NewSet(
	NewSchedulerConfig,
	NewMarketSyncRunner,
	NewScheduledAnalysisRunner,
	NewSchedulerService,
	scheduling_repo.NewScheduledJobRepository,
	scheduling_repo.NewJobExecutionRepository,
	scheduling_repo.NewJobDispatcher,

	// 绑定写在这里而不是 SchedulingAmqpSet 里：Wire 要求 wire.Bind 与
	// 具体类型的 provider 同处一个 set，而 SchedulerService 是这个 set 提供的。
	wire.Bind(new(scheduling_amqp.Executor), new(*scheduling_services.SchedulerService)),
)

// 注意：投递用的 MessagePublisher 绑定不在这里，而在 CoreSet——
// Wire 要求 wire.Bind 与具体类型的 provider 同处一个 set。详见 core_provider.go。

// SchedulingAmqpSet 只在消费端需要：定时任务的执行发生在消息消费者里。
//
// 它不重复包含 SchedulingSet——CoreSet 已经有了，而 Wire 对同一个类型
// 出现两个 provider 会直接报错（这正是它该做的：重复装配意味着两个副本，
// 而两个 SchedulerService 各自注册运行器、各自跑循环，后果不堪设想）。
var SchedulingAmqpSet = wire.NewSet(
	scheduling_amqp.NewJobDueHandler,
)
