package domain_services

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// SchedulerConfig 是调度器的运行参数。
type SchedulerConfig struct {
	// ClaimLimit 是单轮 sweep 最多抢占的任务数。
	ClaimLimit int

	// Parallelism 是单轮 sweep 内并发**投递**的上限。
	//
	// 注意它管的不再是执行。改成消息驱动之后，一轮 sweep 的工作量只剩
	// 「写一条记录、发一条消息」，几乎不碰下游——真正决定下游压力的旋钮
	// 搬到了队列配置的 consumers 上（见 configs/config.yaml）。
	//
	// 那为什么还要并发？因为投递会卡在 broker 上。发布要等确认，
	// 而一个半死不活的 broker 可以让每次发布都耗到超时上限；
	// 串行投递时，50 条到期任务就是 50 倍的超时时长，整轮 sweep 就此停摆。
	// 4 路并发把这个最坏情况压到可接受的范围，而它本身不会给任何下游加压。
	Parallelism int

	// MaxAttempts 是一次触发最多尝试几次（含第一次）。
	//
	// 它是部署参数而不是任务属性，所以长在这里而不是 ScheduledJob 上：
	// 该重试几次取决于下游的脾气（限流恢复得快不快、超时是不是偶发），
	// 而不取决于这条任务本身。给每条任务加一个可调字段，只会多一个没人会去改的旋钮。
	//
	// 与熔断的分工：重试管的是**一次触发内**的临时故障，熔断（连续失败 N 次自动暂停）
	// 管的是**跨触发**的持续故障。三次都失败算作这一次触发失败一次，计入熔断计数。
	MaxAttempts int

	// QueuedGrace 是一条记录停在 queued 多久之后，判定为「消息没送到」。
	//
	// 它要大于「正常情况下消息从发出到被消费的时长」加上重投间隔，
	// 否则恢复巡检会去补投一条其实正躺在重试队列里等着的消息。补投本身是无害的
	// （认领是幂等的），但会制造一堆没有意义的消息和日志。
	QueuedGrace time.Duration

	// RunningGrace 是一条记录停在 running 多久之后，判定为「消费者死了」。
	//
	// 它必须显著大于任务自身的执行超时：一次全市场行情同步跑十几分钟是正常的，
	// 用一个短阈值去判它，等于把正在好好干活的执行强行判死，然后再跑一遍。
	RunningGrace time.Duration

	// RecoverInterval 是恢复巡检的间隔。
	RecoverInterval time.Duration

	// Retention 是执行历史的保留期，供 PurgeExecutions 使用。
	Retention time.Duration
}

func (c SchedulerConfig) normalized() SchedulerConfig {
	if c.ClaimLimit <= 0 {
		c.ClaimLimit = repositories.DefaultClaimLimit
	}
	if c.Parallelism <= 0 {
		c.Parallelism = 4
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.QueuedGrace <= 0 {
		c.QueuedGrace = 5 * time.Minute
	}
	if c.RunningGrace <= 0 {
		c.RunningGrace = 30 * time.Minute
	}
	if c.RecoverInterval <= 0 {
		c.RecoverInterval = time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 90 * 24 * time.Hour
	}
	return c
}

// SchedulerService 编排定时任务的管理用例与执行用例。
//
// 它属于 domain_services/ 而非 application/：这里编排的是完整用例
// （抢占 -> 执行 -> 双聚合各自落库 -> 发事件），只是其中一条用例的触发者
// 是 ticker 而不是 HTTP 请求。application/ 按团队约定只放 handler。
//
// # 两个聚合根，没有共享事务
//
// 一次执行会同时改动 ScheduledJob（失败计数、统计、可能的自动熔断）
// 与 JobExecution（审计记录）。它们**各自独立落库**，没有任何一个事务同时覆盖两者。
// 这是刻意的：即便把两次写入包进一个事务，也覆盖不了中间那段真正耗时的
// 「跑运行器」——不一致的窗口从来就不在数据库里，而在那几分钟的外部调用上。
// 用一个长事务去追求一个它本来就给不了的保证，只会换来跨表的长时间锁持有。
//
// 两者之间的一致性靠**写入顺序**维持：先写任务状态（它决定下次什么时候跑，
// 是系统继续运转的依据），再写审计记录（它只影响可观测性）。
// 若审计记录写失败，代价是历史里少一条；若顺序反过来，任务状态写失败会让
// 下一轮 sweep 的行为出错。两害相权，保状态。
type SchedulerService struct {
	jobs       *repositories.ScheduledJobRepository
	executions *repositories.JobExecutionRepository
	dispatcher *repositories.JobDispatcher
	publisher  domain_event.Publisher
	log        *zap.Logger
	cfg        SchedulerConfig

	// runners 在启动期由组装根注册，之后只读。
	// 仍然加锁的理由：Register 的调用时机由组装根决定，而 Loop 可能已经在跑了；
	// 用一个 RWMutex 把这个假设变成保证，成本是每次查找一次无争用的读锁。
	mu      sync.RWMutex
	runners map[value_objects.JobKind]JobRunner
}

func NewSchedulerService(
	jobs *repositories.ScheduledJobRepository,
	executions *repositories.JobExecutionRepository,
	dispatcher *repositories.JobDispatcher,
	publisher domain_event.Publisher,
	log *zap.Logger,
	cfg SchedulerConfig,
) *SchedulerService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &SchedulerService{
		jobs:       jobs,
		executions: executions,
		dispatcher: dispatcher,
		publisher:  publisher,
		log:        log,
		cfg:        cfg.normalized(),
		runners:    make(map[value_objects.JobKind]JobRunner),
	}
}

// Register 注册一个运行器。由组装根调用，是本上下文获知「怎么干活」的唯一途径。
//
// 后注册的覆盖先注册的，并留一条日志：静默覆盖会让「明明注册了却没生效」
// 变成一个无从查起的问题。
func (s *SchedulerService) Register(runner JobRunner) {
	if runner == nil {
		return
	}
	kind := runner.Kind()
	if !kind.Valid() {
		s.log.Warn("忽略非法种类的任务运行器", zap.String("kind", kind.String()))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runners[kind]; exists {
		s.log.Warn("任务运行器被覆盖注册", zap.String("kind", kind.String()))
	}
	s.runners[kind] = runner
}

func (s *SchedulerService) runnerFor(kind value_objects.JobKind) (JobRunner, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runners[kind]
	return r, ok
}

// ---------------------------------------------------------------------------
// 管理用例
// ---------------------------------------------------------------------------

// CreateJobInput 是创建任务的入参形状。
type CreateJobInput struct {
	Name                   string
	Kind                   string
	Cron                   string
	Payload                map[string]any
	TimeoutSeconds         int
	MaxConsecutiveFailures int
}

// CreateJob 创建一条定时任务。仅管理员。
func (s *SchedulerService) CreateJob(ctx context.Context, op Operator, in CreateJobInput) (*entities.ScheduledJob, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	kind, err := value_objects.NewJobKind(in.Kind)
	if err != nil {
		return nil, err
	}
	if !kind.Valid() {
		return nil, custom_errors.Invalid("必须指定任务种类")
	}
	spec, err := value_objects.NewCronExpression(in.Cron)
	if err != nil {
		return nil, err
	}
	payload, err := value_objects.NewJobPayload(in.Payload)
	if err != nil {
		return nil, err
	}

	// 运行器缺席只警告不阻断：API 进程通常不注册任何运行器（它们跑在 worker 里），
	// 在这里硬性要求存在会让创建接口在 API 侧永远失败。
	if _, ok := s.runnerFor(kind); !ok {
		s.log.Warn("创建的任务种类在本进程没有注册运行器",
			zap.String("kind", kind.String()), zap.String("name", in.Name))
	}

	job, err := entities.Schedule(
		idx.Prefixed("job"), in.Name, kind, spec, payload,
		in.MaxConsecutiveFailures,
		time.Duration(in.TimeoutSeconds)*time.Second,
		op.UserID,
	)
	if err != nil {
		return nil, err
	}
	if err := s.jobs.Create(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// UpdateJobInput 是更新任务的入参形状。零值表示「不改这一项」。
type UpdateJobInput struct {
	Name                   string
	Cron                   string
	Payload                map[string]any
	HasPayload             bool
	TimeoutSeconds         int
	MaxConsecutiveFailures int
}

// UpdateJob 修改任务定义与调度。仅管理员。
//
// 改 cron 走聚合的 UpdateSchedule，它会顺带重算 NextRunAt——
// 只改表达式不改下次执行时间，是这类系统最经典的静默故障。
func (s *SchedulerService) UpdateJob(ctx context.Context, op Operator, jobID string, in UpdateJobInput) (*entities.ScheduledJob, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	job, err := s.jobs.FindByID(ctx, jobID)
	if err != nil {
		return nil, err
	}

	if in.Cron != "" {
		spec, err := value_objects.NewCronExpression(in.Cron)
		if err != nil {
			return nil, err
		}
		if err := job.UpdateSchedule(spec); err != nil {
			return nil, err
		}
	}

	var payload *value_objects.JobPayload
	if in.HasPayload {
		p, err := value_objects.NewJobPayload(in.Payload)
		if err != nil {
			return nil, err
		}
		payload = &p
	}
	if err := job.Reconfigure(in.Name, payload, time.Duration(in.TimeoutSeconds)*time.Second, in.MaxConsecutiveFailures); err != nil {
		return nil, err
	}
	if err := s.jobs.Update(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// DeleteJob 删除任务。仅管理员。执行历史保留，见仓储层注释。
func (s *SchedulerService) DeleteJob(ctx context.Context, op Operator, jobID string) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	return s.jobs.Delete(ctx, jobID)
}

// GetJob 查看单条任务。登录即可。
func (s *SchedulerService) GetJob(ctx context.Context, op Operator, jobID string) (*entities.ScheduledJob, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	return s.jobs.FindByID(ctx, jobID)
}

// ListJobs 分页查询任务。登录即可。
func (s *SchedulerService) ListJobs(
	ctx context.Context, op Operator, kindStr, statusStr string, page shared_vo.Page,
) ([]*entities.ScheduledJob, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	kind, err := value_objects.NewJobKind(kindStr)
	if err != nil {
		return nil, 0, err
	}
	status, err := value_objects.NewJobStatus(statusStr)
	if err != nil {
		return nil, 0, err
	}
	return s.jobs.List(ctx, kind, status, page)
}

// PauseJob 暂停任务。仅管理员。
//
// 直接交给仓储的条件 UPDATE，不走「加载 - 判断 - 保存」：后者是 TOCTOU，
// 两个管理员同时点暂停会双双通过内存判断。判定与写入必须是同一条语句。
func (s *SchedulerService) PauseJob(ctx context.Context, op Operator, jobID string) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	return s.jobs.Pause(ctx, jobID, time.Now())
}

// ResumeJob 恢复任务。仅管理员。
//
// 恢复需要按 cron 重算 NextRunAt，所以不得不先读一次。读写之间的窗口由
// 仓储层的谓词封死（WHERE 同时钉住 status 与 cron_spec），详见 Resume 的注释。
// 这里只负责让聚合做出领域决策，决策的结果由数据库验证是否仍然成立。
func (s *SchedulerService) ResumeJob(ctx context.Context, op Operator, jobID string) error {
	if err := requireAdmin(op); err != nil {
		return err
	}
	job, err := s.jobs.FindByID(ctx, jobID)
	if err != nil {
		return err
	}
	// 读到的表达式就是 CAS 的期望值，必须在 Resume 改动聚合之前取。
	expectedSpec := job.Cron.String()
	if err := job.Resume(); err != nil {
		return err
	}
	return s.jobs.Resume(ctx, jobID, job.NextRunAt, expectedSpec, time.Now())
}

// PreviewCron 推演一个 cron 表达式接下来的 count 次触发时刻。
//
// 存在的理由很朴素：cron 表达式是出了名的容易写错，而写错的后果不是报错，
// 是任务在一个谁也没料到的时间跑（或者永远不跑）。让人在保存之前先看到
// 真实的触发序列，比任何文档都有效。
//
// 它不读库、不改任何状态，因此不需要管理员权限——登录即可。
func (s *SchedulerService) PreviewCron(op Operator, spec string, count int) ([]time.Time, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	expr, err := value_objects.NewCronExpression(spec)
	if err != nil {
		return nil, err
	}
	if count <= 0 {
		count = 5
	}
	next := expr.NextN(time.Now(), count)
	if len(next) == 0 {
		return nil, custom_errors.Invalid("cron 表达式 %q 在未来不会再触发", spec)
	}
	return next, nil
}

// ListExecutions 分页查询某任务的执行历史。登录即可。
func (s *SchedulerService) ListExecutions(
	ctx context.Context, op Operator, jobID string, page shared_vo.Page,
) ([]*entities.JobExecution, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	return s.executions.ListByJob(ctx, jobID, page)
}

// PurgeExecutions 按保留期清理执行历史。仅管理员，也供运维作业调用。
func (s *SchedulerService) PurgeExecutions(ctx context.Context, op Operator) (int64, error) {
	if err := requireAdmin(op); err != nil {
		return 0, err
	}
	return s.executions.PurgeOlderThan(ctx, time.Now().Add(-s.cfg.Retention))
}

// ---------------------------------------------------------------------------
// 执行用例
// ---------------------------------------------------------------------------
//
// ===========================================================================
// 为什么到点不直接执行，而是发一条消息
// ===========================================================================
//
// 「抢到一次触发」和「把这次触发跑完」是两件时长差三个数量级的事：
// 前者是一条 UPDATE，后者可能是十几分钟的行情同步或一次完整的多智能体分析。
// 把它们放在同一个循环里，会同时带来三个问题，而且互相纠缠：
//
//  1. 一个慢任务拖垮整轮巡检。调度循环的并发额度被长任务占着，
//     后面到期的任务只能等，而它们的 next_run_at 早就推进了。
//  2. 失败无处可去。同步执行时，一次失败只能记进日志——重试要么原地
//     阻塞整个循环，要么需要一个地方存放「待重试」这个状态，而那个地方并不存在。
//  3. 扩容的两端被焊死。调度只需要一个副本（多了也是互相抢占），
//     执行需要按下游配额扩容，而同进程意味着两者只能一起加减。
//
// 拆开之后，三件事各自有了归属：巡检永远是毫秒级的；重试由消息队列的
// 延迟重投承担；执行端的副本数与调度端完全无关。
//
// 代价是多了一段真空——记录已写、消息未达。这段真空由那条 queued 记录
// 和恢复巡检共同兜住，详见 RecoverStale。

// TriggerNow 手动触发一次执行。仅管理员。
//
// 手动触发**不消耗**计划内的那一次触发：NextRunAt 原封不动（见聚合的
// MarkManualOccurrence）。管理员点「立即执行」是为了验证任务能不能跑通，
// 没人会预期这一下把当天的自动执行吃掉。
//
// 但它照常参与连续失败计数：一条手动触发也失败的任务，和自动触发失败一样
// 说明目标坏了，没有理由让它绕过熔断。
//
// 返回的是一条 **queued** 记录，不是执行结果：本方法只负责把这次触发排上队。
// 这是刻意的取舍——同步等待意味着一个 HTTP 请求要挂十几分钟，中间任何一次
// 网关超时都会让管理员以为任务失败了，而它其实跑得好好的。
// 想知道结果，去执行历史里看这条记录。
func (s *SchedulerService) TriggerNow(ctx context.Context, op Operator, jobID string) (*entities.JobExecution, error) {
	if err := requireAdmin(op); err != nil {
		return nil, err
	}
	job, err := s.jobs.FindByID(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := job.MarkManualOccurrence(time.Now()); err != nil {
		return nil, err
	}
	return s.enqueue(ctx, job, true)
}

// DispatchDueJobs 执行一轮 sweep：抢占到期任务并把它们排进队列，返回本轮投递的条数。
//
// # 为什么是 Settle 而不是 Map
//
// 扇出必须是**容忍式**的。Map 的失败即取消语义会让一条投递失败把同轮其余任务的
// ctx 一起取消掉，而被取消的那些任务此刻已经**抢占成功**——它们的 next_run_at
// 已经推进，取消它们等于让那几次触发凭空消失。每条任务都必须拿到自己的独立结论，
// 这正是 Settle 的语义。
//
// # 返回值
//
// 只返回致命错误（抢占本身失败）。单条任务的投递失败已经落进了它自己的 queued 记录，
// 恢复巡检会把它捡起来重投，再往上抛一遍只会让调用方在日志里看到两份同样的东西。
func (s *SchedulerService) DispatchDueJobs(ctx context.Context, now time.Time) (int, error) {
	jobs, err := s.jobs.ClaimDue(ctx, now, s.cfg.ClaimLimit)
	if err != nil {
		// ClaimDue 可能在抢到几条之后才失败，已抢到的必须照常投递——
		// 它们的 next_run_at 已经推进了，不投就是丢掉一次触发。
		s.log.Warn("抢占到期定时任务时出错", zap.Error(err))
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	s.log.Info("本轮抢占到期定时任务", zap.Int("count", len(jobs)))

	outcomes, err := concurrency.Settle(ctx, jobs, s.cfg.Parallelism,
		func(ctx context.Context, job *entities.ScheduledJob) (string, error) {
			_, dispatchErr := s.enqueue(ctx, job, false)
			return job.ID, dispatchErr
		})
	if err != nil && ctx.Err() != nil {
		// 停机：已投递的部分已经各自落库，没有需要回滚的东西。
		return len(outcomes), nil
	}
	for i, o := range outcomes {
		if o.Err != nil {
			s.log.Warn("定时任务投递失败，等待恢复巡检补投",
				zap.String("job_id", jobs[i].ID),
				zap.String("job_name", jobs[i].Name),
				zap.Error(o.Err))
		}
	}
	return len(jobs), nil
}

// enqueue 写下欠条并发出消息。这是「一次触发进入系统」的唯一入口。
//
// # 顺序不能反
//
// 先落库、后发消息。反过来的话，消费端有可能在记录写下之前就已经来认领了——
// 它会认领不到任何东西，确认消息然后走人，于是这次触发静默消失。
// 而按现在的顺序，最坏情况只是消息没发出去，那条 queued 记录还在库里欠着，
// 恢复巡检会把它捡起来。
//
// # 为什么发布失败不在这里重试
//
// 因为欠条已经在库里了，它就是重试的依据。在这里再写一层重试，等于让同一件事
// 有两个负责人——而两个负责人的系统，出问题时谁都以为是对方在管。
func (s *SchedulerService) enqueue(ctx context.Context, job *entities.ScheduledJob, manual bool) (*entities.JobExecution, error) {
	exec, err := entities.Enqueue(idx.Prefixed("jobrun"), job.ID, job.Kind, job.ClaimedFor, 1, manual)
	if err != nil {
		return nil, err
	}

	created, err := s.executions.Enqueue(ctx, exec)
	if err != nil {
		return nil, err
	}
	if !created {
		// 这次触发已经有人排过队了（崩溃重启后的恢复巡检最容易撞上）。
		// 消息也一并由那一次负责，这里不必再发一条。
		s.log.Info("这次触发已在队列中，跳过重复投递",
			zap.String("job_id", job.ID), zap.Time("scheduled_for", job.ClaimedFor))
		return exec, nil
	}

	if err := s.dispatchMessage(ctx, exec); err != nil {
		return exec, err
	}
	return exec, nil
}

// dispatchMessage 把一条待执行记录变成一条消息发出去。
func (s *SchedulerService) dispatchMessage(ctx context.Context, exec *entities.JobExecution) error {
	return s.dispatcher.Dispatch(ctx, value_objects.JobDueMessage{
		JobID:        exec.JobID,
		ScheduledFor: exec.ScheduledFor,
		Manual:       exec.Manual,
	})
}

// ExecuteQueued 是消费端的用例：认领一次触发并把它跑完。
//
// 由 application/amqp_handlers 在收到消息时调用。返回错误表示「这条消息该重投」，
// 返回 nil 表示「处理完毕，确认消息」——判据是**重来一次有没有可能成功**。
//
// # 幂等
//
// 投递是至少一次的，同一条消息会到达不止一次。幂等不靠「先查一下跑没跑过」，
// 那是 TOCTOU；靠的是 ClaimQueued 那条把判断写进 WHERE 的 UPDATE：
// 只有命中 1 行的那个副本才会真正执行，其余全部拿到 nil 然后确认走人。
func (s *SchedulerService) ExecuteQueued(ctx context.Context, msg value_objects.JobDueMessage) error {
	exec, err := s.executions.ClaimQueued(ctx, msg.JobID, msg.ScheduledFor)
	if err != nil {
		// 认领失败通常是数据库暂时不可用，重来有可能成功。
		return err
	}
	if exec == nil {
		// 这次触发不欠任何东西了：可能已被别的副本认领，可能早已跑完。
		// 两种情况对这条消息是同一件事——确认然后走人。这是重复投递的正常归宿，
		// 不该打日志，否则每条消息都会在日志里留下一条「无事发生」。
		return nil
	}

	job, err := s.jobs.FindByID(ctx, msg.JobID)
	if err != nil {
		if custom_errors.CodeOf(err) == custom_errors.CodeNotFound {
			// 任务在排队期间被删了。执行记录要收尾（否则它会一直卡在 running
			// 让恢复巡检反复捡起来），但没有任务聚合可以登记结局了。
			s.log.Warn("定时任务已被删除，跳过本次执行", zap.String("job_id", msg.JobID))
			_ = exec.Skip("定时任务已被删除")
			s.finishExecution(ctx, exec)
			return nil
		}
		// 读不到任务但任务还在：这条记录已经是 running 了，直接返回错误让消息重投。
		// 重投时 ClaimQueued 会认领不到（它已是 running），要靠恢复巡检收尾——
		// 这正是 RunningGrace 存在的理由。
		return err
	}

	runner, ok := s.runnerFor(job.Kind)
	if !ok {
		// 本进程没注册这个种类的运行器。这是部署问题，不是任务问题，
		// 所以记成 skipped 而不是 failed——算成失败会让一次发布漏配
		// 在几轮之后把该种类的全部任务集体熔断。
		//
		// 也不重投：重投回到的还是同样配置的这批消费者，结果不会变。
		reason := "本进程未注册 " + job.Kind.String() + " 类型的任务运行器"
		s.log.Error("收到无法处理的定时任务", zap.String("job_id", job.ID), zap.String("kind", job.Kind.String()))
		_ = exec.Skip(reason)
		job.RecordSkipped(time.Now())
		s.persist(ctx, job, exec)
		return nil
	}

	// 超时按任务自身的配置派生。这是唯一能阻止一个卡死的运行器永久占住
	// 一格消费者名额的东西——没有它，几条卡死的任务就能让整个队列停摆。
	runCtx, cancel := context.WithTimeout(ctx, job.EffectiveTimeout())
	defer cancel()

	summary, itemCount, runErr := runner.Run(runCtx, job.Payload)
	finishedAt := time.Now()

	if runErr != nil {
		reason := custom_errors.MessageOf(runErr)
		if reason == "" {
			reason = runErr.Error()
		}
		// 超时的报错信息里通常只有一句 context deadline exceeded，
		// 补上任务自己的超时值，运维才知道该调哪个参数。
		if runCtx.Err() != nil && ctx.Err() == nil {
			reason = "执行超时（上限 " + job.EffectiveTimeout().String() + "）：" + reason
		}
		_ = exec.Fail(reason)
		next, retryErr := s.failAndRetry(ctx, job, exec, finishedAt)
		if retryErr != nil {
			// 欠条没写成。让消息重投，多给这次触发一个机会。
			return retryErr
		}
		if next == nil {
			// 次数用尽。确认这条消息：再重投也认领不到任何记录了。
			// 这次触发的失败已经计入任务的连续失败计数，后续由熔断接管。
			return nil
		}
		// 返回错误让消息队列延迟重投。不在这里自己发一条新消息：那会绕过队列配置好的
		// 重投间隔，变成一次立刻的重试，而立刻重试对「下游整体不可用」这种最常见的
		// 失败原因毫无帮助，只会把失败放大三倍。
		return custom_errors.Unavailable("定时任务 %s 第 %d 次尝试失败，等待重投", job.Name, exec.Attempt)
	}

	_ = exec.Succeed(summary, itemCount)
	job.RecordSuccess(exec.ID, finishedAt, exec.Summary, exec.ItemCount, exec.Duration(), exec.Manual)
	s.persist(ctx, job, exec)
	return nil
}

// failAndRetry 收尾一次失败的尝试，必要时为同一次触发补一张新欠条。
//
// ===========================================================================
// 写入顺序是这段代码里唯一真正要紧的东西
// ===========================================================================
//
// 先补下一次尝试的 queued 记录，**再**把本次结局写死。顺序反过来会开一个
// 无法自愈的窗口：两次写入之间崩溃，这次触发就既不是 running（已被判失败）、
// 也没有下一次尝试（还没写），恢复巡检再也找不到它——一次触发就这么没了，
// 而且不留任何信号。
//
// 按现在的顺序，任何一个崩溃点留下的都是一条 running 或 queued 的记录，
// 而这两种状态恰好就是恢复巡检要找的东西。
//
// # 返回值
//
// 返回新补上的那条待执行记录，次数用尽时返回 (nil, nil)。
// **怎么把这条新记录送出去，由调用方决定**，因为两个调用方的处境不同：
//
//	消费端    原消息还在手上，返回错误让队列延迟重投即可，不必自己发消息
//	恢复巡检  原消息早已被确认掉，不会再有重投，必须自己发一条
//
// 把这个分歧留给调用方，而不是在这里 if 一下，是因为「谁负责送消息」
// 正是两者唯一的区别——用返回值表达它，比用一个 bool 参数表达要诚实。
// # 只有最后一次尝试才登记到任务聚合上
//
// 中间的失败**不碰** ScheduledJob：不加连续失败计数，也不发 OnJobFailed。
//
// 这不是省一次写库，而是语义问题。MaxConsecutiveFailures 是运维按「触发」
// 设定的——写 10 的人想表达的是「连着 10 次该跑没跑成就停了它」。
// 若每次重试都计一笔，max_attempts=3 会让这个阈值实际变成 3 次触发，
// 一个配置项的含义就这样被另一个配置项悄悄改写了。
//
// 事件同理：一次触发失败该发一条 OnJobFailed。发三条意味着用户为同一件事
// 收到三条站内通知，而这三条说的还是同一个原因。
func (s *SchedulerService) failAndRetry(
	ctx context.Context, job *entities.ScheduledJob, exec *entities.JobExecution, finishedAt time.Time,
) (*entities.JobExecution, error) {
	giveUp := func() {
		// 熔断判定在聚合内部完成，这里没有任何 if：规则只能有一个执行点。
		job.RecordFailure(exec.ID, finishedAt, exec.ErrMsg, exec.Manual)
		s.persist(ctx, job, exec)
		s.log.Warn("定时任务执行失败且重试次数已用尽",
			zap.String("job_id", job.ID),
			zap.String("job_name", job.Name),
			zap.Int("attempt", exec.Attempt),
			zap.Int("max_attempts", s.cfg.MaxAttempts),
			zap.String("reason", exec.ErrMsg))
	}

	if !exec.CanRetry(s.cfg.MaxAttempts) {
		giveUp()
		return nil, nil
	}

	next, err := exec.NextAttempt(idx.Prefixed("jobrun"), s.cfg.MaxAttempts)
	if err != nil {
		// CanRetry 已经问过一遍，走到这里说明聚合有别的理由拒绝重试。
		s.log.Warn("无法为失败的执行派生重试", zap.String("job_id", job.ID), zap.Error(err))
		giveUp()
		return nil, nil
	}

	saveCtx, cancel := detach(ctx)
	defer cancel()
	if _, err := s.executions.Enqueue(saveCtx, next); err != nil {
		// 欠条没写成，本次结局也就**不能**写死——让它留在 running 上，
		// 由恢复巡检来收拾。这正是上面那段写入顺序的意义所在。
		s.log.Error("补记重试记录失败，本次执行将由恢复巡检收尾",
			zap.String("job_id", job.ID), zap.Error(err))
		return nil, err
	}

	// 只收尾执行记录，不动任务聚合——理由见上面那段注释。
	s.finishExecution(ctx, exec)
	s.log.Warn("定时任务执行失败，已排入重试",
		zap.String("job_id", job.ID),
		zap.String("job_name", job.Name),
		zap.Int("attempt", exec.Attempt),
		zap.Int("next_attempt", next.Attempt),
		zap.String("reason", exec.ErrMsg))
	return next, nil
}

// RecoverStale 捡回卡住的执行记录，返回本轮处理的条数。
//
// ===========================================================================
// 这是整套消息驱动流程的安全网
// ===========================================================================
//
// 消息驱动引入了一段真空：记录已写、消息未达，或者消息已被认领、消费者却死了。
// 没有这个巡检，落在真空里的触发就永远停在那里——而它们的表现是**安静的**：
// 任务状态正常、next_run_at 照常推进、没有任何错误日志，只是那几次没跑。
//
// 两种卡法，处置完全不同：
//
//	queued 卡太久   消息没送到。记录本身没问题，补发一条消息即可。
//	running 卡太久  消费者在执行途中死了。没有人会再给它写结局，
//	                必须由巡检判它失败，并按重试策略补一次尝试。
//
// # 为什么补发消息是安全的
//
// 因为认领是幂等的。若原消息其实还躺在重试队列里，两条消息最终只有一条能认领成功，
// 另一条拿到 nil 然后确认走人。宁可多发一条消息，也不要漏掉一次触发。
func (s *SchedulerService) RecoverStale(ctx context.Context) (int, error) {
	now := time.Now()
	stale, err := s.executions.ListStale(ctx,
		now.Add(-s.cfg.QueuedGrace), now.Add(-s.cfg.RunningGrace), s.cfg.ClaimLimit)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	s.log.Warn("发现卡住的定时任务执行记录", zap.Int("count", len(stale)))

	// 同样用 Settle：一条记录处置失败不该让同轮其余记录失去机会。
	outcomes, err := concurrency.Settle(ctx, stale, s.cfg.Parallelism,
		func(ctx context.Context, exec *entities.JobExecution) (string, error) {
			return exec.ID, s.recoverOne(ctx, exec)
		})
	if err != nil && ctx.Err() != nil {
		return len(outcomes), nil
	}
	for i, o := range outcomes {
		if o.Err != nil {
			s.log.Error("卡住的执行记录处置失败",
				zap.String("execution_id", stale[i].ID),
				zap.String("job_id", stale[i].JobID),
				zap.Error(o.Err))
		}
	}
	return len(stale), nil
}

func (s *SchedulerService) recoverOne(ctx context.Context, exec *entities.JobExecution) error {
	if exec.Status == value_objects.ExecutionStatusQueued {
		s.log.Warn("待执行记录长时间无人认领，补发消息",
			zap.String("execution_id", exec.ID),
			zap.String("job_id", exec.JobID),
			zap.Time("queued_at", exec.QueuedAt))
		return s.dispatchMessage(ctx, exec)
	}

	// running 卡住：执行它的那个消费者已经不在了。
	reason := "执行超过 " + s.cfg.RunningGrace.String() + " 未收尾，判定为消费者中途退出"
	if err := exec.Fail(reason); err != nil {
		return err
	}

	job, err := s.jobs.FindByID(ctx, exec.JobID)
	if err != nil {
		if custom_errors.CodeOf(err) == custom_errors.CodeNotFound {
			// 任务已删：收尾这条记录就够了，没有聚合可以登记结局。
			s.finishExecution(ctx, exec)
			return nil
		}
		return err
	}
	// 走与消费端完全相同的失败路径，包括写入顺序、重试判定、以及
	// 「只有最后一次尝试才登记到任务聚合上」——两条路径共用同一段代码，
	// 是它们不会在某次改动后悄悄产生分歧的唯一保证。
	next, err := s.failAndRetry(ctx, job, exec, time.Now())
	if err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	// 与消费端的分歧只在这一句：那条卡死记录的原消息早就被确认掉了
	// （消费者是死在确认之后的），不会再有任何重投。消息必须由这里补发。
	return s.dispatchMessage(ctx, next)
}

// Loop 是常驻的调度循环，供 worker 子命令调用，阻塞直到 ctx 取消。
//
// 两件事在同一个循环里按各自的节奏跑：投递到期任务（interval），
// 以及捡回卡住的记录（RecoverInterval）。合在一个 select 里而不是起两个 goroutine，
// 是因为它们都是「到点扫一轮」的串行动作，各自耗时都在毫秒级，
// 而两个 goroutine 就意味着调用方要负责等两个东西退出。
//
// 返回 nil 而不是 ctx.Err()：正常停机不是错误，让调用方不必在退出路径上
// 特判 context.Canceled。
//
// # tick 间隔与调度精度
//
// 触发精度上限就是 interval：间隔 30 秒意味着任务最多晚 30 秒被**投递**
// （被执行还要再加上队列等待）。不追求秒级精度是有意的——把 interval 压到 1 秒，
// N 个副本就会每秒各发一次候选查询，而其中绝大多数轮次一条到期任务都没有。
// 真实的延迟由 job_executions 的 scheduled_for / queued_at / started_at
// 三列如实记录，需要的时候可以分段算出来。
func (s *SchedulerService) Loop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	dispatchTicker := time.NewTicker(interval)
	defer dispatchTicker.Stop()
	recoverTicker := time.NewTicker(s.cfg.RecoverInterval)
	defer recoverTicker.Stop()

	s.log.Info("定时任务调度循环已启动",
		zap.Duration("interval", interval),
		zap.Int("parallelism", s.cfg.Parallelism),
		zap.Int("claim_limit", s.cfg.ClaimLimit),
		zap.Int("max_attempts", s.cfg.MaxAttempts),
		zap.Duration("recover_interval", s.cfg.RecoverInterval))
	defer s.log.Info("定时任务调度循环已停止")

	for {
		select {
		case <-ctx.Done():
			return nil
		case tick := <-dispatchTicker.C:
			if _, err := s.DispatchDueJobs(ctx, tick); err != nil && ctx.Err() == nil {
				s.log.Warn("定时任务巡检出错", zap.Error(err))
			}
		case <-recoverTicker.C:
			if _, err := s.RecoverStale(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("卡住记录的恢复巡检出错", zap.Error(err))
			}
		}
	}
}

// persist 把两个聚合各自落库，然后发布它们累积的事件。
//
// 两次写入**不共享事务**，理由见类型注释。顺序也是刻意的：
// 任务状态决定系统接下来怎么走，审计记录只影响可观测性，所以先保状态。
//
// 落库脱离上游取消：停机时这次执行已经真的发生了（可能已经花掉几分钟的外部调用），
// 结局必须写下去，否则连续失败计数永远推不到阈值，熔断就形同虚设。
func (s *SchedulerService) persist(ctx context.Context, job *entities.ScheduledJob, exec *entities.JobExecution) {
	saveCtx, cancel := detach(ctx)
	defer cancel()

	if err := s.jobs.SaveRunOutcome(saveCtx, job); err != nil {
		s.log.Error("定时任务执行结果落库失败",
			zap.String("job_id", job.ID), zap.Error(err))
	}
	if err := s.executions.Finish(saveCtx, exec); err != nil {
		s.log.Error("任务执行记录落库失败",
			zap.String("job_id", job.ID),
			zap.String("execution_id", exec.ID),
			zap.Error(err))
	}

	// 事件在领域决策与落库都完成之后才发布：先发事件会让消费者看到一个
	// 库里还查不到的执行记录。
	if evts := job.GetAllPendingEvents(); len(evts) > 0 {
		if err := s.publisher.Publish(saveCtx, evts...); err != nil {
			s.log.Error("定时任务领域事件发布失败",
				zap.String("job_id", job.ID), zap.Error(err))
		}
	}
}

// finishExecution 只收尾执行记录，用于任务聚合已经不存在的情况。
func (s *SchedulerService) finishExecution(ctx context.Context, exec *entities.JobExecution) {
	saveCtx, cancel := detach(ctx)
	defer cancel()
	if err := s.executions.Finish(saveCtx, exec); err != nil {
		s.log.Error("任务执行记录落库失败",
			zap.String("execution_id", exec.ID), zap.Error(err))
	}
}

// detach 返回一个继承了 ctx 的值、但不受其取消影响的 context，并自带超时。
// 与 analysis.WorkerService 的同名函数用意相同：收尾写入必须跑完，
// 但「脱离取消」不等于「允许无限期阻塞」。
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
