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
		// 必须显著大于**最长的那条任务**的超时，不是大于「一般任务」的超时。
		//
		// 取 2 小时是被日线同步逼出来的：没有批量按日端点时它退回逐标的路径，
		// A 股 5900 只 × EastmoneyRPS 2 次/秒 ≈ 50 分钟，而下面 defaultJobs 里
		// 给它的超时是 7200 秒。用 30 分钟去判它，恢复巡检会把一条正在好好干活的
		// 执行判成「消费者死了」，补投一条新的——于是两条同时在跑，一起去撞
		// sync_runs 的 running_key 唯一索引，后一条必然失败。
		RunningGrace:    2 * time.Hour,
		RecoverInterval: time.Minute,
	}
}

// seedCreatedBy 是添加任务记在 created_by 上的用户 ID。
//
// scheduled_jobs.created_by 刻意没有外键（跨上下文只按标识引用），所以这个值
// 不需要真实存在；它只是审计列，约定 1 号为初始管理员。而 entities.Schedule
// 拒绝 0（「任务必须记录创建人」），因此这里不能用零值表达「系统创建」。
const seedCreatedBy uint64 = 1

// defaultJobs 是新环境起来就该有的行情同步任务。
//
// # 为什么要添加，而不是让运维手动建
//
// 建表 migration 没有种子数据，唯一的创建入口是管理接口。结果是每套新环境
// （本地、测试、生产）都要有人记得手动建一遍同样的三条任务，漏建的表现不是报错
// 而是**什么都不发生**——调度器每 30 秒扫一次，扫到 0 行，日志上安静得像一切正常。
//
// # cron 按 Asia/Shanghai 解释
//
// robfig/cron 用 time.Local 算下次触发，而 Dockerfile 里 ENV TZ=Asia/Shanghai。
// 这两件事必须同时成立，时刻才对得上 A 股交易时段；改镜像时区等于改这张表的含义。
//
// # 顺序不是摆设
//
// stock_list 排在最前：stocks 表为空时 listSymbols 返回 404「没有可同步的标的」，
// 后两条会整条失败。三条之间没有任何编排，靠 cron 时刻拉开间距。
var defaultJobs = []scheduling_services.CreateJobInput{
	{
		Name: "CN-股票列表-每日",
		Kind: "market_sync",
		// 开盘前跑：新股上市当天就该在名单里，否则当天的行情与 K 线会因为
		// 没有本地锚点被整批丢掉（见 syncQuotesBatch 的 universe 过滤）。
		Cron:           "30 8 * * 1-5",
		Payload:        map[string]any{"kind": "stock_list", "market": "CN"},
		TimeoutSeconds: 600,
	},
	{
		Name: "CN-行情快照-每日",
		Kind: "market_sync",
		// A 股 15:00 收盘，留半小时给东财的收盘数据落定。
		Cron:           "30 15 * * 1-5",
		Payload:        map[string]any{"kind": "quotes", "market": "CN"},
		TimeoutSeconds: 900,
	},
	{
		Name: "CN-日线-每日",
		Kind: "market_sync",
		// 排在行情之后：两条都要打同一个数据源，岔开时刻是为了不让它们
		// 共享同一个令牌桶互相饿死。
		Cron:    "0 16 * * 1-5",
		Payload: map[string]any{"kind": "klines", "market": "CN"},
		// 两小时，不是一般任务那种十分钟：有批量按日端点时这条只要几分钟，
		// 没有时退回逐标的路径就是 5900 次调用约 50 分钟。超时按后者给，
		// 否则这条任务在降级路径上永远跑不完。
		TimeoutSeconds: 7200,
	},
}

// SeedDefaultJobs 添加 defaultJobs 里缺席的任务，返回这次新建了几条。
//
// 只补不改（见 SchedulerService.EnsureJob）：已存在的任务连看都不看一眼，
// 运维改过的 cron、暂停状态都不会被重启抹掉。反过来说，**改这张表里的 cron
// 不会影响已经建好的环境**——那属于改已有任务，走管理接口。
//
// 单条失败不阻断其余：一条 cron 写错不该让另外两条也播不进去，那会把一次
// 手误放大成「整个环境没有任何定时任务」。
func SeedDefaultJobs(ctx context.Context, svc *scheduling_services.SchedulerService, log *zap.Logger) int {
	created := 0
	for _, in := range defaultJobs {
		job, isNew, err := svc.EnsureJob(ctx, in, seedCreatedBy)
		if err != nil {
			log.Error("添加默认定时任务失败",
				zap.String("name", in.Name), zap.Error(err))
			continue
		}
		if !isNew {
			continue
		}
		created++
		log.Info("已添加默认定时任务",
			zap.String("name", job.Name), zap.String("cron", job.Cron.String()),
			zap.Time("next_run_at", job.NextRunAt))
	}
	return created
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
