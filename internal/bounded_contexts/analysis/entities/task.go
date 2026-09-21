// Package entities 承载分析上下文的全部业务不变式。
//
// 本包不感知 HTTP、Redis、数据库与事务——那些分别属于 application/ 与 repositories/。
// 本上下文有两个聚合根：Task 与 Batch，它们只通过 ID 互相引用，各自独立落库。
package entities

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Task 是分析上下文的聚合根，覆盖排队、执行、完成的完整生命周期。
//
// 字段全部导出，与团队既有服务保持一致：不变式靠「唯一的写入方法」来守，
// 而不是靠私有字段 + 一堆 getter。任何状态迁移都必须经由下面的意图方法，
// 这样「终态不可再迁移」「进度快照与状态始终自洽」两条规则只有一个执行点。
//
// BatchID 是对另一个聚合根 Batch 的引用，只存 ID，不存指针：
// 跨聚合只按标识引用，否则每推进一个子任务都要把整个批次加载进内存。
type Task struct {
	domain_event.EventRecorder

	ID      string
	UserID  uint64
	BatchID string
	Request value_objects.Request

	Status   value_objects.Status
	Progress value_objects.Progress

	// Result 是终局产出，仅在 Status == completed 时有值。
	Result *value_objects.Result
	ErrMsg string

	Attempts int
	// StateChangedAt 是当前状态的进入时刻，由 setStatus 统一维护。
	//
	// 它与 StartedAt 分工不同，不要混用：StartedAt 是首次启动时刻、重试不覆盖，
	// 服务于 Duration() 对外报的总耗时；StateChangedAt 每次迁移都刷新，
	// 回答的是「这一行维持当前状态多久了」，是停滞巡检唯一的依据。
	StateChangedAt time.Time
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// setStatus 是全部状态迁移的唯一出口。
//
// 状态和「何时迁到这个状态」必须一起写。分开写迟早有一条迁移路径漏掉时间戳，
// 而漏掉的表现是**安静的**：没有报错、没有日志，只是停滞巡检会把一个刚刚
// 重新排队的任务按老时间戳判成卡死，然后把它判失败。让两者同进同出，
// 这类疏漏就变成了不可能，而不是需要靠 review 逐条盯住。
func (t *Task) setStatus(s value_objects.Status, at time.Time) {
	t.Status = s
	t.StateChangedAt = at
}

// NewTask 创建一个排队中的分析任务。这是任务进入系统的唯一入口。
func NewTask(id string, userID uint64, req value_objects.Request) (*Task, error) {
	if id == "" {
		return nil, custom_errors.Invalid("任务 ID 不能为空")
	}
	if userID == 0 {
		return nil, custom_errors.Invalid("任务必须归属于一个用户")
	}
	// Request 由 VO 构造器保证合法，这里只兜底「根本没给代码」这种调用错误。
	if req.IsZero() {
		return nil, custom_errors.Invalid("分析请求不完整：缺少股票代码")
	}

	now := time.Now()
	t := &Task{
		ID:        id,
		UserID:    userID,
		Request:   req,
		Progress:  value_objects.NewProgress(req),
		CreatedAt: now,
	}
	t.setStatus(value_objects.StatusQueued, now)
	t.AddDomainEvent(domain_events.NewOnTaskQueued(
		id, userID, "", req.Code.FullSymbol(), req.Depth.Int(), false))
	return t, nil
}

// JoinBatch 把任务挂到批次上。只允许在排队阶段挂载：
// 批次的结算集合依赖「子任务集合在批次创建后不再变化」这条假设。
func (t *Task) JoinBatch(batchID string) error {
	if batchID == "" {
		return custom_errors.Invalid("批次 ID 不能为空")
	}
	if t.Status != value_objects.StatusQueued {
		return custom_errors.Conflict("任务已开始执行，无法加入批次")
	}
	t.BatchID = batchID
	return nil
}

// Start 将任务置为运行中。
//
// 重复 Start 视为重投递，允许幂等重入：崩溃恢复本来就会把同一个任务再投一次，
// 若这里报错，一次消费者崩溃就会让任务永久卡死。
//
// 「同一时刻只有一个消费者能开工」不由本方法保证，也无法由它保证——内存里的
// 检查挡不住两个进程。那条保证来自仓储的 ClaimTask：它把 status='queued'
// 写进 UPDATE 的 WHERE，让检查与写入成为同一个原子操作。
func (t *Task) Start() error {
	if t.Status.Terminal() {
		return custom_errors.Conflict("任务已处于终态 %s，无法启动", t.Status.DisplayName())
	}
	now := time.Now()
	t.setStatus(value_objects.StatusRunning, now)
	t.Attempts++
	if t.StartedAt == nil {
		// 只记首次启动时间，重试不覆盖：Duration() 表达的是「用户等了多久」。
		t.StartedAt = &now
	}
	t.Progress = t.Progress.WithMessage("分析中")
	t.AddDomainEvent(domain_events.NewOnTaskStarted(t.ID, t.UserID, t.Attempts))
	return nil
}

// Complete 以成功收尾。
func (t *Task) Complete(result *value_objects.Result) error {
	if t.Status.Terminal() {
		return custom_errors.Conflict("任务已处于终态 %s，无法完成", t.Status.DisplayName())
	}
	if result == nil {
		// 没有产出的「成功」是自相矛盾的状态：下游报告生成会拿到 nil。
		// 在这里拦住，好过让它变成一条 status=completed 但 result 为 NULL 的脏数据。
		return custom_errors.Invalid("分析结果为空，无法标记任务完成")
	}
	now := time.Now()
	t.setStatus(value_objects.StatusCompleted, now)
	t.Result = result
	t.ErrMsg = ""
	t.FinishedAt = &now
	// Progress 不可变，因此是把新快照替换回来，而不是原地改。
	t.Progress = t.Progress.MarkDone()
	t.AddDomainEvent(domain_events.NewOnTaskCompleted(
		t.ID, t.UserID, t.BatchID, t.Request.Code.FullSymbol(),
		result.Decision.Action.String(), result.Decision.Confidence,
		decimal.NewFromInt(int64(t.Duration()/time.Second))))
	return nil
}

// Fail 以失败收尾。
//
// maxAttempts 参与事件构造而不只是参与 Retryable 判定：批次侧必须知道
// 「这次失败还会不会重试」。若把一次可恢复的失败也登记进批次失败计数，
// 重试成功后批次的 completed+failed 会超过 total，结算数直接对不上。
//
// 传 0 表示「没有任何人会重试这次失败」——提交阶段入队失败就是这种情况：
// 任务根本没进队列，不会有 worker 来重试它，事件里必须如实标成不可重试，
// 否则批次会在等一个永远不会到来的结局。
func (t *Task) Fail(reason string, maxAttempts int) error {
	if t.Status.Terminal() {
		return custom_errors.Conflict("任务已处于终态 %s，无法标记失败", t.Status.DisplayName())
	}
	if reason == "" {
		reason = "分析执行失败"
	}
	now := time.Now()
	t.setStatus(value_objects.StatusFailed, now)
	t.ErrMsg = reason
	t.FinishedAt = &now
	t.Progress = t.Progress.WithMessage("分析失败")
	t.AddDomainEvent(domain_events.NewOnTaskFailed(
		t.ID, t.UserID, t.BatchID, reason, t.Attempts, t.Retryable(maxAttempts)))
	return nil
}

// Cancel 只允许取消尚未进入终态的任务。
func (t *Task) Cancel() error {
	if t.Status.Terminal() {
		return custom_errors.Conflict("任务已处于终态 %s，无法取消", t.Status.DisplayName())
	}
	now := time.Now()
	t.setStatus(value_objects.StatusCanceled, now)
	t.FinishedAt = &now
	t.Progress = t.Progress.WithMessage("已取消")
	t.AddDomainEvent(domain_events.NewOnTaskCanceled(t.ID, t.UserID, t.BatchID))
	return nil
}

// Retryable 判断失败任务是否还能重投递。
func (t *Task) Retryable(maxAttempts int) bool {
	return t.Status == value_objects.StatusFailed && t.Attempts < maxAttempts
}

// Requeue 将失败任务重置回排队态。
func (t *Task) Requeue() error {
	if t.Status.Final() {
		// 已完成/已取消是用户可见的终局，重新排队会让结果凭空变化。
		// 失败则不同：它是一次未竟的尝试，重试是合理的。
		return custom_errors.Conflict("任务状态 %s 不支持重新排队", t.Status.DisplayName())
	}
	t.setStatus(value_objects.StatusQueued, time.Now())
	t.ErrMsg = ""
	t.FinishedAt = nil
	t.Progress = t.Progress.WithMessage("重新排队中")
	t.AddDomainEvent(domain_events.NewOnTaskQueued(
		t.ID, t.UserID, t.BatchID, t.Request.Code.FullSymbol(), t.Request.Depth.Int(), true))
	return nil
}

// AdvanceProgress 推进一个步骤。
//
// 进度是聚合状态的一部分，因此由聚合持有并替换，而不是让外部拿到 Progress 自己改——
// Progress 是不可变值对象，外部改出来的新快照根本回不到任务里。
// 运行中之外的状态忽略推进：终态任务再收到迟到的进度回调会让百分比倒退。
func (t *Task) AdvanceProgress(key, detail string) {
	if t.Status != value_objects.StatusRunning {
		return
	}
	t.Progress = t.Progress.Advance(key, detail)
}

// FailProgressStep 标记某步骤失败，但不改变任务状态——
// 单个分析师失败是否导致整个任务失败，由 Fail 单独决定。
func (t *Task) FailProgressStep(key, detail string) {
	if t.Status != value_objects.StatusRunning {
		return
	}
	t.Progress = t.Progress.FailStep(key, detail)
}

// Duration 返回任务的执行时长；未开始为 0，未结束按「到此刻」计算。
func (t *Task) Duration() time.Duration {
	if t.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	return end.Sub(*t.StartedAt)
}

// OwnedBy 判断任务归属，供 domain_services/ 做越权校验。
func (t *Task) OwnedBy(userID uint64) bool { return t.UserID == userID }
