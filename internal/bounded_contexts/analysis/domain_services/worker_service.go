package domain_services

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// WorkerConfig 是消费端的运行参数。
type WorkerConfig struct {
	// ReconcileInterval 是批次对账的巡检间隔。
	ReconcileInterval time.Duration
	// ReconcileGrace 是批次「安静多久之后才允许被对账」的宽限期。
	ReconcileGrace time.Duration
	// ReconcileBatchSize 是单轮对账最多处理的批次数。
	ReconcileBatchSize int
	// ReconcileParallelism 是对账扇出的并行上限。
	ReconcileParallelism int
	// QueuedGrace 是任务停在 queued 多久之后，判定为「消息没送到」。
	QueuedGrace time.Duration
	// RunningGrace 是任务停在 running 多久之后，判定为「消费者死了」。
	//
	// 它必须大于「一次分析可能跑多久」的上限，否则巡检会把健康的长任务判死——
	// 杀掉一个正在跑的付费任务，正是这套机制要避免的事。
	RunningGrace time.Duration
}

func (c WorkerConfig) normalized() WorkerConfig {
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 5 * time.Minute
	}
	if c.ReconcileGrace <= 0 {
		c.ReconcileGrace = 10 * time.Minute
	}
	if c.ReconcileBatchSize <= 0 {
		c.ReconcileBatchSize = 50
	}
	if c.ReconcileParallelism <= 0 {
		c.ReconcileParallelism = 4
	}
	if c.QueuedGrace <= 0 {
		// 排队等待是正常的（并发闸门会压着任务不放），宽限期要大于常见的排队时长，
		// 否则巡检会给还在正常排队的任务反复补发消息。
		c.QueuedGrace = 10 * time.Minute
	}
	if c.RunningGrace <= 0 {
		// 取 broker 的 consumer_timeout 同量级（默认 30 分钟）：超过它，消息本来
		// 就会被 broker 判超时收回，任务不可能还在合法地跑。两个数字量化的是
		// 同一件事，调其中一个就要一起调。
		c.RunningGrace = 30 * time.Minute
	}
	return c
}

// WorkerService 是分析任务的队列消费端。
//
// 它属于 domain_services/ 而非 application/：这里编排的是一条完整用例
// （出队 -> 启动 -> 跑引擎 -> 收尾 -> 确认），只是触发者是队列而不是 HTTP 请求。
// application/ 按团队约定只放 handler。
type WorkerService struct {
	tasks      *repositories.TaskRepository
	batches    *repositories.BatchRepository
	dispatcher *repositories.TaskDispatcher
	progress   *repositories.ProgressPublisher
	guard      *repositories.ConcurrencyGuard
	batchSvc   *BatchService
	engine     Engine
	publisher  domain_event.Publisher
	log        *zap.Logger
	policy     Policy
	cfg        WorkerConfig
}

func NewWorkerService(
	tasks *repositories.TaskRepository,
	batches *repositories.BatchRepository,
	dispatcher *repositories.TaskDispatcher,
	progress *repositories.ProgressPublisher,
	guard *repositories.ConcurrencyGuard,
	batchSvc *BatchService,
	engine Engine,
	publisher domain_event.Publisher,
	log *zap.Logger,
	policy Policy,
	cfg WorkerConfig,
) *WorkerService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &WorkerService{
		tasks: tasks, batches: batches, dispatcher: dispatcher, progress: progress,
		guard: guard, batchSvc: batchSvc, engine: engine, publisher: publisher,
		log: log, policy: policy.normalized(), cfg: cfg.normalized(),
	}
}

// Run 启动两个巡检循环，阻塞直到 ctx 取消。
//
// # 这里为什么不再有消费循环
//
// 任务的取用已经交给消息队列：消费者由 application/amqp_handlers 注册，
// 到货时回调 RunTask。本服务不再自己「取一个、做一个」，并发度由队列声明里的
// Consumers 决定，而不是由这里起几个循环决定。
//
// 剩下的两个循环都是**兜底**，不在任务的正常路径上：
//
//	staleLoop      捡回卡住的任务（认领赢家崩了、消息丢了）
//	reconcileLoop  补齐批次结算（批次与子任务不共享事务的代价）
//
// # 为什么这里不写 go func()
//
// 扇出在本服务里只有一个合法入口：helpers/concurrency。手写 goroutine + WaitGroup
// 的问题不是风格，而是它每次都要重新实现一遍并行上限、ctx 传播、错误聚合和取消传播，
// 其中任意一条写漏都是线上事故。
//
// 返回 nil 而不是 ctx.Err()：正常停机不是错误，让调用方不必在退出路径上
// 特判 context.Canceled。
func (w *WorkerService) Run(ctx context.Context) error {
	loops := []func(context.Context) error{w.reconcileLoop, w.staleLoop}

	w.log.Info("分析任务巡检已启动",
		zap.Int("max_attempts", w.policy.MaxAttempts))

	err := concurrency.ForEach(ctx, loops, len(loops),
		func(ctx context.Context, loop func(context.Context) error) error {
			return loop(ctx)
		})

	w.log.Info("分析任务巡检已停止")
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// RunTask 执行一个任务的完整生命周期，由消息队列的处理器在消息到货时调用。
//
// # 返回值就是投递语义
//
// 返回错误表示「请重投这条消息」，返回 nil 表示「确认，这条消息没有后续了」。
// 判据只有一句：**重来一次有没有可能成功**。
//
// 业务失败一律返回 nil，看着反直觉，但它是对的：重试语义由聚合的 Attempts 与
// Requeue 决定，finishFailed 在还有余量时会亲自派发一条**新**消息。此时若再
// 返回错误，同一次重试就会有两条消息，而队列的重投计数还会绕过领域的最大尝试次数——
// 一个必然失败的任务会被重投到死信为止，白烧掉好几轮 LLM 调用。
//
// 真正该返回错误的只有「还没开工就出岔子」：认领时数据库不可用。那既不是业务失败，
// 也没有产生任何新消息，只有靠重投才能挽回。
func (w *WorkerService) RunTask(ctx context.Context, taskID string) error {
	// 认领与开工是同一个原子动作，不是「先查再改」：投递是至少一次的，
	// 同一个任务可能在本消费者还没跑完时就被重新投递给另一个消费者。
	// 判据写在 ClaimTask 的 WHERE 里，只有命中 1 行的那个副本才真正开工。
	task, err := w.tasks.ClaimTask(ctx, taskID)
	if err != nil {
		// 认领没跑通通常是数据库暂时不可用，重来有可能成功：让消息重投。
		w.log.Warn("认领任务失败", zap.String("task_id", taskID), zap.Error(err))
		return err
	}
	if task == nil {
		// 没认领到：任务不存在、已被别的消费者接手、或早已进入终态
		// （Cancel 不去队列里删消息，陈旧消息都从这里退场）。
		// 重复投递下这是正常归宿，确认走人即可，不打日志。
		// 名额由认领方或取消路径负责归还，凭据保证不会重复扣减。
		return nil
	}

	// 名额归还的判定放在这里统一收口，而不是散在各个收尾分支：
	// 只有任务真正进入终态才归还。重试会把任务放回队列，而重投递路径上没有
	// Acquire，提前归还等于让重试绕过并发配额。
	// 重复归还是安全的（凭据机制），因此这里不必和取消路径互相提防。
	defer func() {
		if task.Status.Terminal() {
			w.release(ctx, task.UserID, task.ID)
		}
	}()

	w.publish(ctx, task)
	w.pushProgress(ctx, task)

	result, runErr := w.runEngine(ctx, task)

	if runErr != nil {
		w.finishFailed(ctx, task, runErr)
		return nil
	}
	if err := task.Complete(result); err != nil {
		w.finishFailed(ctx, task, err)
		return nil
	}
	// 收尾落库脱离上游取消：停机时任务已经跑完了，结果必须写下去。
	saveCtx, cancelSave := detach(ctx)
	defer cancelSave()
	if err := w.tasks.Update(saveCtx, task); err != nil {
		// 结果已经产出却没写进去，是这条路径上唯一真正的坏情况。
		// 仍然返回 nil：重投会让整条分析从头再跑一遍（十几轮 LLM 调用），
		// 而库里那一行还停在 running，停滞巡检会按重试策略正经地处置它。
		w.log.Error("任务完成状态落库失败", zap.String("task_id", task.ID), zap.Error(err))
		return nil
	}
	w.publish(ctx, task)
	w.pushProgress(ctx, task)
	return nil
}

// runEngine 跑一次引擎，返回它的产出。
//
// # 这里为什么只剩一行
//
// 它曾经是一次扇出：引擎与「可见性续约」两个工作单元并排跑、一起收场。续约存在的
// 唯一理由是 Redis 队列的可见性超时——不定期续约，任务就会被回收巡检误判成崩溃并重投。
//
// 换到消息队列之后这个理由整个消失了：一条未 ack 的消息本来就不会被投给别人，
// 不需要任何人定期声明「我还活着」。于是扇出、取消域、以及它们之间那些必须精确对齐的
// 时序，一并没有了存在的必要。
//
// 这段历史值得留着，因为删掉的不只是代码：那次扇出里藏过一个 bug——引擎成功后自己
// 取消共享 ctx，而 ForEach 会把这个自我取消判成整轮失败，于是十四步全部跑完、报告都
// 生成了的任务，照样以 context canceled 收尾并一路重试到上限。能写出那个 bug 的地方
// 现在不存在了。
//
// # 这里为什么必须自己设 deadline
//
// 传进来的 ctx 是消费者的生命周期 ctx，它没有 deadline——只有进程退出才会取消它。
// 也就是说，如果不在这里加一层，AnalysisMaxRuntime 这个「领域上限」就只是一句注释：
// 三个从它派生出来的超时里，只有巡检的 RunningGrace 真的在跑，而巡检干的事情是
// **重投**。于是一次卡死的分析会走成这样：跑过 35 分钟 → RecoverStale 判定进程已死
// → Requeue → Dispatch → 另一个消费者 ClaimTask 成功（库里那行已经回到 queued）
// → 同一个分析开始第二次并发执行，而第一次还在烧 LLM 调用，没有任何东西会叫停它。
//
// 加上 deadline 之后，引擎在上限处自己收到取消、正常走 finishFailed，重试次数由
// 聚合的 Attempts 管着，LLM 开销有上界。scheduling 那边的 runJob 一直是这么做的
// （context.WithTimeout(ctx, job.EffectiveTimeout())），这里只是把同一件事补齐。
// 用 AnalysisRunDeadline 而不是 AnalysisMaxRuntime，是为了抢在 broker 前面收尾，
// 理由见那个常量。
func (w *WorkerService) runEngine(ctx context.Context, task *entities.Task) (*value_objects.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, constants.AnalysisRunDeadline)
	defer cancel()
	return w.engine.Run(runCtx, task.ID, task.Request, &taskReporter{worker: w, ctx: runCtx, task: task})
}

// finishFailed 以失败收尾，并在还有重试余量时重新排队。
func (w *WorkerService) finishFailed(ctx context.Context, task *entities.Task, cause error) {
	reason := custom_errors.MessageOf(cause)
	if reason == "" {
		reason = cause.Error()
	}
	if err := task.Fail(reason, w.policy.MaxAttempts); err != nil {
		w.log.Warn("标记任务失败被拒绝", zap.String("task_id", task.ID), zap.Error(err))
		return
	}
	saveCtx, cancelSave := detach(ctx)
	defer cancelSave()
	if err := w.tasks.Update(saveCtx, task); err != nil {
		w.log.Error("任务失败状态落库失败", zap.String("task_id", task.ID), zap.Error(err))
		return
	}
	w.publish(ctx, task)
	w.pushProgress(ctx, task)
	w.log.Warn("分析任务失败",
		zap.String("task_id", task.ID),
		zap.Int("attempts", task.Attempts),
		zap.Error(cause))

	// 重试判定是领域决策，由聚合给出。
	if !task.Retryable(w.policy.MaxAttempts) {
		return
	}
	if err := task.Requeue(); err != nil {
		return
	}
	if err := w.tasks.Update(saveCtx, task); err != nil {
		w.log.Error("任务重新排队落库失败", zap.String("task_id", task.ID), zap.Error(err))
		return
	}
	if err := w.dispatcher.Dispatch(saveCtx, task.ID); err != nil {
		w.log.Error("任务重新派发失败", zap.String("task_id", task.ID), zap.Error(err))
		return
	}
	w.publish(ctx, task)
}

// staleLoop 周期性捡回卡住的任务。
func (w *WorkerService) staleLoop(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := w.RecoverStale(ctx); err != nil && ctx.Err() == nil {
				w.log.Warn("停滞任务巡检失败", zap.Error(err))
			}
		}
	}
}

// RecoverStale 捞出卡住的任务并逐个处置，返回本轮处理的条数。
//
// 导出是为了让进程启动时能先跑一轮：上一次是被强杀停掉的话，那批 running
// 要等一整个巡检周期才被捡起来，而重启恰恰是最可能留下它们的时刻。
//
// 用 Settle 而不是 Map：一个任务处置失败不该让同轮其余任务失去机会，
// 每个都要有独立结论。并行有上限——巡检跑在业务库上，不能因为积压了几百条
// 就把数据库压垮。
func (w *WorkerService) RecoverStale(ctx context.Context) (int, error) {
	now := time.Now()
	stale, err := w.tasks.ListStale(ctx,
		now.Add(-w.cfg.QueuedGrace), now.Add(-w.cfg.RunningGrace), w.cfg.ReconcileBatchSize)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	w.log.Warn("发现卡住的分析任务", zap.Int("count", len(stale)))

	outcomes, err := concurrency.Settle(ctx, stale, w.cfg.ReconcileParallelism,
		func(ctx context.Context, t *entities.Task) (string, error) {
			return t.ID, w.recoverOne(ctx, t)
		})
	if err != nil && ctx.Err() != nil {
		return len(outcomes), nil
	}
	for _, o := range outcomes {
		if o.Err != nil {
			w.log.Error("卡住的分析任务处置失败",
				zap.String("task_id", o.Value), zap.Error(o.Err))
		}
	}
	return len(outcomes), nil
}

// recoverOne 处置一个卡住的任务。两种卡法，处置完全不同。
func (w *WorkerService) recoverOne(ctx context.Context, task *entities.Task) error {
	switch task.Status {
	case value_objects.StatusQueued:
		// 消息没送到（提交后发布失败、broker 丢了、消费者集体不在）。
		// 任务本身没问题，补一条消息即可。
		//
		// 补发是安全的，因为认领是幂等的：万一原消息其实还躺在重试队列里，
		// 两条消息最终只有一条能认领成功，另一条拿到 nil 然后确认走人。
		// 宁可多发一条，也不要漏掉一个任务——漏掉的表现是它永远停在「排队中」。
		return w.dispatcher.Dispatch(ctx, task.ID)

	case value_objects.StatusRunning:
		// 认领成功之后消费者死了，没有人会再给它写结局。
		// 判它失败，剩下的交给重试策略：finishFailed 会在还有余量时重新排队。
		//
		// 理由写成用户能看懂的一句话，而不是让它落成一个笼统的「服务内部错误」——
		// 这条 error 会直接出现在任务详情和站内通知里。
		w.finishFailed(ctx, task, custom_errors.Internal(
			"分析执行中断：任务已运行超过 %s 仍无进展，判定为执行进程异常退出",
			w.cfg.RunningGrace))
		return nil

	default:
		// 状态在「捞出来」和「处置」之间已经变了，交给下一轮。
		return nil
	}
}

// reconcileLoop 周期性对账批次，是「批次与子任务不共享事务」这个决定的兜底。
//
// 它捞出「尚未结算完且已安静一段时间」的批次，逐个补齐缺失的结算。
// 对账天生是按批次逐条处理的，因此这里用 concurrency.Settle 做扇出：
//   - Settle 而不是 Map：一个坏批次不该中断整轮巡检，每个批次要有独立结论；
//   - 有并行上限：对账跑在业务库上，不能因为积压了几百个批次就压垮数据库。
func (w *WorkerService) reconcileLoop(ctx context.Context) error {
	if w.batchSvc == nil {
		return nil
	}
	ticker := time.NewTicker(w.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			stale, err := w.batches.ListStale(ctx, w.cfg.ReconcileGrace, w.cfg.ReconcileBatchSize)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				w.log.Warn("查询待对账批次失败", zap.Error(err))
				continue
			}
			if len(stale) == 0 {
				continue
			}
			outcomes, err := concurrency.Settle(ctx, stale, w.cfg.ReconcileParallelism,
				func(ctx context.Context, b *entities.Batch) (string, error) {
					return b.ID, w.batchSvc.Reconcile(ctx, b.ID)
				})
			if err != nil && ctx.Err() != nil {
				return nil
			}
			for i, o := range outcomes {
				if o.Err != nil {
					w.log.Warn("批次对账失败",
						zap.String("batch_id", stale[i].ID), zap.Error(o.Err))
				}
			}
		}
	}
}

// pushProgress 广播当前进度快照。
// 推送失败只记日志：进度是尽力而为的展示信息，让它失败回滚一次分析是本末倒置。
func (w *WorkerService) pushProgress(ctx context.Context, task *entities.Task) {
	pubCtx, cancel := detach(ctx)
	defer cancel()
	if err := w.progress.Publish(pubCtx, task.ID, task.Progress); err != nil {
		w.log.Debug("推送任务进度失败", zap.String("task_id", task.ID), zap.Error(err))
	}
}

func (w *WorkerService) release(ctx context.Context, userID uint64, taskIDs ...string) {
	relCtx, cancel := detach(ctx)
	defer cancel()
	_, _ = w.guard.Release(relCtx, userID, taskIDs...)
}

func (w *WorkerService) publish(ctx context.Context, t *entities.Task) {
	if evts := t.GetAllPendingEvents(); len(evts) > 0 {
		_ = w.publisher.Publish(ctx, evts...)
	}
}

// taskReporter 把引擎的进度回调翻译成「更新聚合 + 广播」。
//
// 它不直接改 Progress：Progress 是不可变值对象，改出来的新快照必须由
// Task 聚合收下才算数，否则推送出去的进度和库里的状态会分叉。
type taskReporter struct {
	worker *WorkerService
	ctx    context.Context
	task   *entities.Task
}

var _ ProgressReporter = (*taskReporter)(nil)

func (r *taskReporter) Step(key, detail string) {
	r.task.AdvanceProgress(key, detail)
	r.flush()
}

func (r *taskReporter) StepFailed(key, detail string) {
	r.task.FailProgressStep(key, detail)
	r.flush()
}

// flush 只推 Redis，不写 MySQL。
//
// 进度每步都落库会让 analysis_tasks 变成一张高频写表（一次分析十几次 UPDATE），
// 而进度的唯一消费者是实时订阅的前端。库里的进度在每次状态迁移时一并写入，
// 足够支撑「刷新页面看到大致到哪一步」。
func (r *taskReporter) flush() {
	r.worker.pushProgress(r.ctx, r.task)
}

// detach 返回一个继承了 ctx 的值、但不受其取消影响的 context，并自带超时。
//
// 停机时正在收尾的写操作必须跑完：任务已经花掉几分钟的 LLM 调用，
// 若因为 ctx 被取消而写不进结果，这笔消耗就白花了。
// 超时是必要的——脱离取消不等于允许无限期阻塞。
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// sleepCtx 在 ctx 未取消的前提下休眠，返回 false 表示应当退出。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
