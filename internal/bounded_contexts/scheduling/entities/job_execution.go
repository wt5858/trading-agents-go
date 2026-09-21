package entities

import (
	"strings"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// maxSummaryLen 与仓储层 summary 列的宽度对齐；超长的摘要在这里截断，
// 让「写不进去」这件事在领域层就被解决掉，而不是变成一个数据库报错。
const maxSummaryLen = 512

// JobExecution 是一次执行尝试，**它是独立的聚合根，不是 ScheduledJob 的子实体**。
//
// ===========================================================================
// 它不只是审计记录，它还是一张欠条
// ===========================================================================
//
// 调度器抢到一次触发之后并不立刻执行，而是发一条消息到队列、由消费端来跑。
// 这中间有一段真空：消息可能发失败，进程可能在发出之前被杀。
// 若此时库里什么都没有，那一次触发就凭空消失了——而 next_run_at 已经推进，
// 表面上一切正常，只是那一次没跑。这是最难发现的一类故障，因为它没有任何信号。
//
// 所以记录在**抢到触发的那一刻**就以 queued 状态写下，此时消息还没发。
// 这条 queued 记录同时是三样东西：
//
//	欠条    「这次触发欠着没跑」，恢复巡检据此补投
//	抢占对象 消费端把 queued 改成 running，命中 1 行才算拿到这次执行
//	审计起点 它最终会变成 succeeded / failed，留在历史里
//
// # 一次尝试 = 一条记录
//
// 重试不是改写这条记录，而是新建下一条（Attempt 加一）。
// 失败的尝试必须原样留在历史里：重试三次的任务如果只看得到最后一次，
// 前两次为什么失败就无从查起了。
//
// ===========================================================================
// 为什么不把它做成 ScheduledJob 的子实体
// ===========================================================================
//
// # 一、规模：子实体意味着「加载整条历史才能追加一行」
//
// 一条每分钟执行的任务一年产生约 52 万条执行记录。如果 JobExecution 是
// ScheduledJob 的一部分，那么按 DDD 的规则，追加一次执行必须先把整个聚合
// （含全部历史）加载进内存、在内存里 append、再整体写回。
// 这不是性能调优问题，而是聚合边界划错了：追加一条审计记录根本不需要
// 知道之前的 52 万条长什么样。
//
// # 二、不变式：它的规则不涉及任务状态
//
// 聚合边界应当由**一致性边界**划定，而不是由「看起来属于谁」划定。
// JobExecution 要守的规则只有一条：终态不可改写（一条审计记录写下就不能翻供）。
// 这条规则完全在它自己内部，不需要读 ScheduledJob 的任何字段。
// 反过来，ScheduledJob 的核心不变式（连续失败 N 次自动熔断）靠的是它自己的
// ConsecutiveFailures 计数器，也不需要去数执行记录。
// 两个聚合的不变式互不相交 —— 这正是「可以拆」的判据。
//
// # 三、归档：独立聚合才能独立清理
//
// 执行记录有独立的生命周期：保留 90 天后即可删除（PurgeOlderThan）。
// 作为子实体，删除历史就成了「修改 ScheduledJob 聚合」，一次归档作业
// 会去碰调度器每个 tick 都要扫的热表。独立成根之后，清理只碰 job_executions。
//
// ===========================================================================
// 引用方式
// ===========================================================================
//
// JobID 是对另一个聚合根的**标识引用**，不是指针。跨聚合只按 ID 引用，
// 两张表之间也刻意不建外键——外键会把它们绑进同一个事务，
// 而「一个事务同时写两个聚合根」正是这里要避免的东西。
type JobExecution struct {
	domain_event.EventRecorder

	ID    string
	JobID string
	// JobKind 是写入时刻的快照，不是实时读出来的。
	// 执行记录是审计，必须反映「当时」的事实；任务后来被改掉不该让历史记录跟着变。
	JobKind value_objects.JobKind

	// 三个时刻各自回答一个不同的问题，少存一个就有一段延迟永远算不出来：
	//
	//	ScheduledFor  计划什么时候跑        —— 由 cron 定下
	//	QueuedAt      调度器什么时候发现的  —— 减去 ScheduledFor 就是调度延迟
	//	StartedAt     消费端什么时候开始跑  —— 减去 QueuedAt 就是队列积压时长
	//
	// 换成消息驱动之后，第二段（队列积压）是新出现的、也是最需要盯的一段：
	// 它变长意味着消费者不够用，而这件事在旧的同步执行模型里根本不存在。
	ScheduledFor time.Time
	QueuedAt     time.Time
	StartedAt    time.Time
	FinishedAt   *time.Time

	// Attempt 是这次触发的第几次尝试，从 1 开始。
	// 它与 (JobID, ScheduledFor) 一起构成一次尝试的天然主键，
	// 数据库上的唯一索引因此能挡住重复投递造成的重复入队。
	Attempt int

	Status    value_objects.ExecutionStatus
	Summary   string
	ItemCount int
	ErrMsg    string

	// DurationMs 是派生量（FinishedAt - StartedAt），随记录一起落库。
	// 读路径直接取：执行记录是历史，耗时是写入当时固化的事实，
	// 在读路径上用 time.Since 重算会让一条未完成的记录每刷新一次就变长。
	DurationMs int64

	// Manual 区分自动 sweep 与管理员手动触发。
	// 排查「这条任务昨天为什么跑了两次」时，这一列是第一个要看的东西。
	Manual bool
}

// Enqueue 记下「这次触发欠着没跑」。这是执行记录进入系统的唯一入口。
//
// 调用时机是调度器刚抢到一次触发、消息还没发出去的那一刻。顺序不能反：
// 先发消息再记录，消费端有可能在记录写下之前就已经来认领了。
//
// StartedAt 此刻先跟 QueuedAt 对齐，真正开始执行时由 Start 改写。
// 让它一开始就有值而不是留空，是为了让历史列表的「按开始时间倒序」
// 对 queued 记录同样成立——一条排不进序的记录会沉到列表最底下，
// 而它恰恰是最新、最该被看见的那条。
func Enqueue(
	id, jobID string, kind value_objects.JobKind,
	scheduledFor time.Time, attempt int, manual bool,
) (*JobExecution, error) {
	if id == "" {
		return nil, custom_errors.Invalid("执行记录 ID 不能为空")
	}
	if jobID == "" {
		return nil, custom_errors.Invalid("执行记录必须归属于一个定时任务")
	}
	if scheduledFor.IsZero() {
		return nil, custom_errors.Invalid("执行记录必须带有计划触发时刻")
	}
	if attempt <= 0 {
		attempt = 1
	}
	now := time.Now()
	return &JobExecution{
		ID:      id,
		JobID:   jobID,
		JobKind: kind,
		// 对齐到毫秒是必须的，不是洁癖。scheduled_for 参与唯一索引，也是消费端
		// 认领这次执行时 WHERE 里的等值条件；而库里的列是 datetime(3)。
		// 不对齐的话，写进去的值会被数据库四舍五入到毫秒，而消息里带的还是纳秒，
		// 于是那条等值查询永远查不到——表现为「消息收到了，但认领不到任何记录」，
		// 一次触发就这样静默消失。
		ScheduledFor: scheduledFor.Truncate(time.Millisecond),
		QueuedAt:     now,
		StartedAt:    now,
		Attempt:      attempt,
		Status:       value_objects.ExecutionStatusQueued,
		Manual:       manual,
	}, nil
}

// Start 把一条待执行记录推进到执行中。
//
// 状态判断在这里，是为了让「只有 queued 才能开始」这条规则有唯一的执行点。
// 但请注意：**并发安全不由这个方法提供**。两个消费者同时读到同一条 queued 记录时，
// 两边的内存判断都会通过——这是典型的 TOCTOU，光靠实体方法挡不住。
// 真正的保证来自仓储层 ClaimQueued 那条把判断写进 WHERE 的 UPDATE，
// 而本方法负责的是让领域规则可读、可测，以及挡住调用顺序写错的情况。
func (e *JobExecution) Start() error {
	if e.Status != value_objects.ExecutionStatusQueued {
		return custom_errors.Conflict("执行记录处于 %s，不能开始执行", e.Status.DisplayName())
	}
	e.Status = value_objects.ExecutionStatusRunning
	e.StartedAt = time.Now()
	return nil
}

// NextAttempt 为同一次触发派生下一次尝试。
//
// 「什么情况下允许重试」是业务规则，因此判定在聚合里而不是在处理器里：
//
//	只有失败的尝试才重试   成功的没必要，跳过的重试也还是跳过（运行器仍然没注册）
//	次数用尽就不再重试     否则一个永远失败的目标会让同一次触发无限重投
//
// maxAttempts 由调用方传入而不是长在聚合上：它是部署参数（跟下游的脾气走），
// 不是这条任务自身的属性，为它加一列数据库字段只会多一个没人会去改的旋钮。
//
// 返回的新记录是 queued 状态，由调用方落库后再发消息——顺序与 Enqueue 一致。
func (e *JobExecution) NextAttempt(id string, maxAttempts int) (*JobExecution, error) {
	if e.Status != value_objects.ExecutionStatusFailed {
		return nil, custom_errors.Conflict("只有失败的执行才能重试，当前为 %s", e.Status.DisplayName())
	}
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if e.Attempt >= maxAttempts {
		return nil, custom_errors.Conflict("执行已尝试 %d 次，达到上限 %d", e.Attempt, maxAttempts)
	}
	return Enqueue(id, e.JobID, e.JobKind, e.ScheduledFor, e.Attempt+1, e.Manual)
}

// CanRetry 在不构造新记录的情况下回答「还能不能重试」。
// 供调用方在决定要不要发重投消息时先问一句，避免拿构造失败当分支条件用。
func (e *JobExecution) CanRetry(maxAttempts int) bool {
	return e.Status == value_objects.ExecutionStatusFailed && e.Attempt < maxAttempts
}

// Succeed 以成功收尾。
//
// itemCount 由运行器给出（同步了多少条、清理了多少行）。它是运维看板上
// 唯一能回答「这次跑出效果了吗」的数字——一次「成功但处理了 0 条」的同步
// 和一次失败同样值得注意。
func (e *JobExecution) Succeed(summary string, itemCount int) error {
	if err := e.requireRunning(); err != nil {
		return err
	}
	if itemCount < 0 {
		itemCount = 0
	}
	e.Status = value_objects.ExecutionStatusSucceeded
	e.Summary = truncate(summary, maxSummaryLen)
	e.ItemCount = itemCount
	e.ErrMsg = ""
	e.finish()
	return nil
}

// Fail 以失败收尾。
func (e *JobExecution) Fail(reason string) error {
	if err := e.requireRunning(); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "任务执行失败"
	}
	e.Status = value_objects.ExecutionStatusFailed
	e.ErrMsg = truncate(reason, maxSummaryLen)
	e.finish()
	return nil
}

// Skip 记录一次「抢到了但没跑」。
//
// 之所以要为跳过留一条记录而不是什么都不写：NextRunAt 已经被推进了，
// 这一次触发在调度器看来已经用掉。若不留痕，运维看到的就是
// 「任务状态正常、下次执行时间也在往前走，但执行历史里什么都没有」——
// 这是最难排查的一类故障，因为它没有任何错误信号。
func (e *JobExecution) Skip(reason string) error {
	if err := e.requireRunning(); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "任务被跳过"
	}
	e.Status = value_objects.ExecutionStatusSkipped
	e.Summary = truncate(reason, maxSummaryLen)
	e.finish()
	return nil
}

// Duration 返回执行耗时，直接读固化值，不重算。
func (e *JobExecution) Duration() time.Duration {
	return time.Duration(e.DurationMs) * time.Millisecond
}

// Delay 返回调度延迟：调度器发现这次触发的时刻 - 计划触发时刻。
//
// 上限就是调度循环的巡检间隔。它变大只意味着一件事：巡检没跑或跑不过来。
func (e *JobExecution) Delay() time.Duration {
	if e.ScheduledFor.IsZero() || e.QueuedAt.IsZero() {
		return 0
	}
	return e.QueuedAt.Sub(e.ScheduledFor)
}

// QueueWait 返回消息在队列里排了多久：开始执行时刻 - 入队时刻。
//
// 这是换成消息驱动之后新出现的一段延迟，也是最值得盯的一段：
// 它持续变大说明消费者不够用，而调度延迟（Delay）对此毫无反应——
// 只看 Delay 的话，一个已经堵了半小时的队列看起来和健康时一模一样。
func (e *JobExecution) QueueWait() time.Duration {
	if e.QueuedAt.IsZero() || e.Status == value_objects.ExecutionStatusQueued {
		return 0
	}
	return e.StartedAt.Sub(e.QueuedAt)
}

// BelongsTo 供领域服务做归属校验，避免「用任务 A 的 ID 查任务 B 的历史」。
func (e *JobExecution) BelongsTo(jobID string) bool { return e.JobID == jobID }

// requireRunning 把「只有执行中的记录才能收尾」收口到一处。
//
// 它守着本聚合的两条不变式：
//
//	终态不可改写   一条审计记录写下结论就不能翻供，否则「上周那次失败」
//	              可能在今天变成成功，审计就失去了意义
//	没开始就不能收尾 一条还在 queued 的记录直接被判成失败，意味着它的消息
//	              还在队列里，而历史里已经写着「跑过了」——两边对不上
func (e *JobExecution) requireRunning() error {
	if e.Status.Terminal() {
		return custom_errors.Conflict("执行记录已处于终态 %s，不可改写", e.Status.DisplayName())
	}
	if e.Status != value_objects.ExecutionStatusRunning {
		return custom_errors.Conflict("执行记录处于 %s，尚未开始执行", e.Status.DisplayName())
	}
	return nil
}

// finish 统一收尾：打时间戳并固化耗时。
func (e *JobExecution) finish() {
	now := time.Now()
	e.FinishedAt = &now
	e.DurationMs = now.Sub(e.StartedAt).Milliseconds()
	if e.DurationMs < 0 {
		// 时钟回拨（NTP 校准）会让差值为负。记 0 而不是一个负耗时：
		// 负数会让下游的平均耗时统计变成负值，比丢失精度危险得多。
		e.DurationMs = 0
	}
}

// truncate 按字节截断，与数据库列宽对齐。
// 运行器的摘要可能来自上游的错误文案，长度完全不可控。
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// 按 rune 边界回退，避免把一个多字节汉字切成半个导致列里出现乱码。
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start 判断一个字节是否是 UTF-8 编码的起始字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
