package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// settleMaxRetries 是乐观锁冲突时的重试上限。
//
// 批次上限 100，同一批次上的并发结算者最多也就是几个 worker，
// 3 次重试足以穿过正常的争用；再多就说明有别的问题，不该在这里死等。
const settleMaxRetries = 3

// BatchService 协调 Task 与 Batch 两个聚合根。
//
// ===========================================================================
// 为什么这里没有事务：两个聚合根不共享事务
// ===========================================================================
//
// 旧实现有一个 CreateBatchWithTasks，在一个数据库事务里同时写 analysis_batches
// 与 analysis_tasks。那是把两个聚合根绑进同一个一致性边界，代价是：
// 批次一旦大起来，这个事务会长时间持有两张表的锁；而且它给人一种虚假的安全感——
// 事务只覆盖了数据库，覆盖不了紧接着的 Redis 入队与名额占用，
// 真正的不一致窗口从来就没被消掉过。
//
// 现在两个聚合各存各的：BatchRepository.Create 写一行批次，
// TaskRepository.SaveAll 写 N 行任务，各自原子，互不参与对方的事务。
// 它们之间靠领域事件 + 幂等结算串起来。
//
// ===========================================================================
// 一致性故事：批次落库成功、子任务落库失败会怎样
// ===========================================================================
//
// 写入顺序是「先批次、后子任务」。这个顺序是刻意的：
// 批次行里存着它声明的全部子任务 ID，因此**批次行本身就是一份意图声明**。
// 只要它落库了，系统就永远知道「本来应该有哪 N 个任务」，不需要任何外部日志。
//
// 于是失败被分成两种，都能收敛：
//
//  1. 进程还活着（SaveAll 或 DispatchMany 返回了错误）。
//     degrade() 立刻做**前向补偿**：把这一批子任务在内存里判失败、一条 UPDATE 批量落盘、
//     在批次聚合上把它们结算成 failed、归还名额、发出 OnBatchDegraded。
//     结果：批次的 total 不变，completed+failed 立刻等于 total，批次是自洽且已结束的。
//     用户看到的是「这批全失败了」，而不是一个永远卡在 0% 的批次。
//     注意这不是回滚——已经落库的行不会被删除，它们被如实标记成失败。
//     回滚需要跨资源的原子性（MySQL + Redis），那是我们一开始就不打算买的东西。
//
//  2. 进程在两步之间被杀（批次行在库里，子任务一行没写）。
//     此时没有任何代码在跑，谈不上补偿。修复交给 Reconcile()：
//     它读批次、读该批次实际落库的任务、把「声明了但不存在」的 ID 结算成失败，
//     把「存在且已终态」的 ID 按实际结局结算。worker 的巡检循环会周期性调用它。
//
// ===========================================================================
// 为什么这是幂等且可对账的
// ===========================================================================
//
//   - 所有 ID 由调用方预先生成，因此重试写的是同一批主键；
//     BatchRepository.Create 与 TaskRepository.SaveAll 都是 ON CONFLICT DO NOTHING，
//     重放一次 = 什么都没发生。
//   - 结算是集合的并集而不是自增：Batch.Settle 先查 SettledTaskIDs 再计数，
//     同一个子任务被结算多少次都只算一次。所以事件投递只需要「至少一次」，
//     不需要「恰好一次」——这正是敢用领域事件代替分布式事务的前提。
//   - 并发结算靠 version 乐观锁：写回命中 0 行就重新加载再来一遍，
//     而重放之所以安全，正是因为 Settle 幂等。
//   - 名额归还靠 Redis 凭据，DEL 的返回值保证恰好一次，
//     补偿路径和 worker 收尾路径同时触发也不会多扣。
//   - 因此 Reconcile 可以被任意次数、任意时刻重复调用：它只会把缺失的结算补齐，
//     不会把已经对的东西改坏。「可重复执行的对账程序」是这套设计的兜底，
//     也是它敢放弃跨聚合事务的理由。
type BatchService struct {
	tasks      *repositories.TaskRepository
	batches    *repositories.BatchRepository
	dispatcher *repositories.TaskDispatcher
	guard      *repositories.ConcurrencyGuard
	publisher  domain_event.Publisher
	policy     Policy
}

func NewBatchService(
	tasks *repositories.TaskRepository,
	batches *repositories.BatchRepository,
	dispatcher *repositories.TaskDispatcher,
	guard *repositories.ConcurrencyGuard,
	publisher domain_event.Publisher,
	policy Policy,
) *BatchService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &BatchService{
		tasks: tasks, batches: batches, dispatcher: dispatcher,
		guard: guard, publisher: publisher, policy: policy.normalized(),
	}
}

// SubmitBatchInput 是批量提交的入参形状。Codes 之外的字段对批内所有任务生效。
type SubmitBatchInput struct {
	Codes     []string
	Market    string
	TradeDate string
	Depth     int
	Analysts  []string
	LLMModel  string
}

// BatchOutput 是批量提交的产出。
type BatchOutput struct {
	Batch *entities.Batch
	Tasks []*entities.Task
}

// SubmitBatch 批量提交分析。
//
// 全程没有「每个任务一次 RPC」的循环：配额一次性申请 N 个（一条 Lua）、
// 批次一条 INSERT、子任务分片多值 INSERT、入队一条 LPUSH、事件攒成一批发布。
// 提交 50 只股票的网络往返是常数级而非 O(N)。
func (s *BatchService) SubmitBatch(ctx context.Context, op Operator, in SubmitBatchInput) (*BatchOutput, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	if len(in.Codes) == 0 {
		return nil, custom_errors.Invalid("批量分析至少需要一只股票")
	}
	if len(in.Codes) > entities.MaxBatchSize {
		return nil, custom_errors.Invalid("单批次最多 %d 只股票，当前 %d 只",
			entities.MaxBatchSize, len(in.Codes))
	}

	// 先把全部请求校验完再动任何外部资源：让一只非法代码在申请配额之前就被拒，
	// 而不是占了 50 个名额之后才发现第 17 只代码有问题。
	reqs := make([]value_objects.Request, 0, len(in.Codes))
	for _, code := range in.Codes {
		req, err := s.buildRequest(SubmitInput{
			Code: code, Market: in.Market, TradeDate: in.TradeDate,
			Depth: in.Depth, Analysts: in.Analysts, LLMModel: in.LLMModel,
		})
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}

	batchID := idx.BatchID()
	tasks := make([]*entities.Task, 0, len(reqs))
	taskIDs := make([]string, 0, len(reqs))
	for _, req := range reqs {
		task, err := entities.NewTask(idx.TaskID(), op.UserID, req)
		if err != nil {
			return nil, err
		}
		if err := task.JoinBatch(batchID); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
		taskIDs = append(taskIDs, task.ID)
	}

	batch, err := entities.NewBatch(batchID, op.UserID, taskIDs)
	if err != nil {
		return nil, err
	}

	// 一条 Lua 原子申请 N 个名额：要么全拿到，要么一个都不拿。
	// 此时还没碰数据库，失败可以直接返回，没有任何需要补偿的东西。
	if err := s.guard.Acquire(ctx, op.UserID, taskIDs, s.policy.Limits.PerUser, s.policy.Limits.Global); err != nil {
		return nil, err
	}

	// 第一步：批次单独落库。它是意图声明，必须先于子任务存在——
	// 否则子任务写了一半而批次不存在，就真的无从对账了。
	if err := s.batches.Create(ctx, batch); err != nil {
		s.release(ctx, op.UserID, taskIDs...)
		return nil, err
	}
	s.publishBatch(ctx, batch)

	// 第二步：子任务单独落库，与批次不共享事务。
	if err := s.tasks.SaveAll(ctx, tasks); err != nil {
		s.degrade(ctx, batch, tasks, "子任务落库失败")
		return nil, err
	}

	// 第三步：入队。落库在前、入队在后——worker 出队后第一件事就是按 ID 读任务。
	if err := s.dispatcher.DispatchMany(ctx, taskIDs); err != nil {
		s.degrade(ctx, batch, tasks, "任务派发失败")
		return nil, err
	}

	s.publishTasks(ctx, tasks)
	return &BatchOutput{Batch: batch, Tasks: tasks}, nil
}

// GetBatch 按 ID 取批次，附带归属校验。
func (s *BatchService) GetBatch(ctx context.Context, op Operator, batchID string) (*entities.Batch, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	batch, err := s.batches.FindByID(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if !op.IsAdmin && !batch.OwnedBy(op.UserID) {
		// 同 Task：用 NotFound 而不是 Forbidden，避免批次 ID 空间可被探测。
		return nil, custom_errors.NotFound("分析批次(id=%s) 不存在", batchID)
	}
	return batch, nil
}

// ListBatches 列出用户的批次。
func (s *BatchService) ListBatches(ctx context.Context, op Operator, targetUserID uint64, page shared_vo.Page) ([]*entities.Batch, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	if targetUserID == 0 {
		targetUserID = op.UserID
	}
	if targetUserID != op.UserID && !op.IsAdmin {
		return nil, 0, custom_errors.Forbidden("无权查看其他用户的分析批次")
	}
	return s.batches.ListByUser(ctx, targetUserID, page)
}

// SettleTask 把一个子任务的结局登记进它所属的批次。
//
// 这是 Task 与 Batch 之间唯一的连接点，由领域事件驱动（见
// application/domain_event_handlers/batch_settlement_handler.go）。
// 两个聚合因此各写各的，靠一条「至少一次」的事件串起来。
//
// 幂等：Batch.Settle 先查已结算集合，重复投递只算一次，所以本方法可以被任意重放。
// 并发：写回用乐观锁；命中 0 行说明有别的结算者抢先了，重新加载后再来一遍。
func (s *BatchService) SettleTask(ctx context.Context, batchID, taskID string, outcome entities.SettleOutcome) error {
	if batchID == "" || taskID == "" {
		return nil
	}
	for attempt := 0; attempt < settleMaxRetries; attempt++ {
		batch, err := s.batches.FindByID(ctx, batchID)
		if err != nil {
			return err
		}
		if !batch.Settle(taskID, outcome) {
			// 不属于本批次、已结算过，或批次早已结束——都是无害的重放，直接返回。
			return nil
		}
		err = s.batches.Save(ctx, batch)
		if err == nil {
			s.publishBatch(ctx, batch)
			return nil
		}
		if custom_errors.CodeOf(err) != custom_errors.CodeConflict {
			return err
		}
		// 乐观锁冲突：丢弃这份聚合，下一轮重新加载。这里的重试是争用重试，
		// 不是「按条目循环发 RPC」，次数有上限且与批次大小无关。
	}
	return custom_errors.Conflict("分析批次(id=%s) 结算争用过于频繁，请稍后重试", batchID)
}

// Reconcile 对账一个批次，把缺失的结算补齐。
//
// 用于进程在「批次已落库 / 子任务未落库」之间被杀的场景，也用于兜底任何
// 丢失的结算事件。它是可重复执行的：只把尚未结算的子任务按实际情况补上，
// 已经对的部分一律不动。
//
// 整个过程只有两次查询（批次 + 该批次的全部任务），没有按子任务循环发 RPC。
func (s *BatchService) Reconcile(ctx context.Context, batchID string) error {
	batch, err := s.batches.FindByID(ctx, batchID)
	if err != nil {
		return err
	}
	pending := batch.PendingTaskIDs()
	if len(pending) == 0 {
		return nil
	}

	live, err := s.tasks.ListByBatch(ctx, batchID)
	if err != nil {
		return err
	}
	byID := make(map[string]*entities.Task, len(live))
	for _, t := range live {
		byID[t.ID] = t
	}

	var orphaned []string
	changed := false
	for _, id := range pending {
		task, ok := byID[id]
		if !ok {
			// 批次声明了它，但它从来没能落库：判失败，名额随后归还。
			orphaned = append(orphaned, id)
			continue
		}
		switch {
		case task.Status == value_objects.StatusCompleted:
			changed = batch.Settle(id, entities.SettleCompleted) || changed
		case task.Status.Terminal() && !task.Retryable(s.policy.MaxAttempts):
			// failed 且已无重试余量，或 canceled：这个子任务到此为止。
			changed = batch.Settle(id, entities.SettleFailed) || changed
		}
	}
	if len(orphaned) > 0 {
		if dropped := batch.Drop(orphaned, "子任务未能落库"); len(dropped) > 0 {
			changed = true
			s.release(ctx, batch.UserID, dropped...)
		}
	}
	if !changed {
		return nil
	}
	if err := s.batches.Save(ctx, batch); err != nil {
		// 冲突说明有并发结算者正在推进同一个批次，对账下一轮再来即可。
		if custom_errors.CodeOf(err) == custom_errors.CodeConflict {
			return nil
		}
		return err
	}
	s.publishBatch(ctx, batch)
	return nil
}

// degrade 是批量提交的前向补偿：把这一批子任务如实判失败，让批次立刻自洽并结束。
//
// 它不是回滚——已经落库的行不会被删掉。回滚需要 MySQL 与 Redis 之间的原子性，
// 那是我们一开始就不打算买的东西。相比之下，「把没能开工的任务记成失败」
// 既是真实发生的事，又能让批次的 completed+failed 立刻补齐到 total，
// 不留一个永远停在 0% 的批次。
//
// 全程只有常数次 RPC：一条批量 UPDATE、一条批次 UPDATE、一条名额归还。
func (s *BatchService) degrade(ctx context.Context, batch *entities.Batch, tasks []*entities.Task, reason string) {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
		// maxAttempts 传 0：这些任务根本没进队列，不会有人重试它们。
		_ = t.Fail(reason, 0)
	}
	// 尽力而为：行可能压根没写进去，带 IN 的 UPDATE 自然会略过不存在的行。
	_ = s.tasks.FailAll(ctx, tasks, reason)

	if dropped := batch.Drop(ids, reason); len(dropped) > 0 {
		if err := s.batches.Save(ctx, batch); err == nil {
			s.publishBatch(ctx, batch)
		}
		s.release(ctx, batch.UserID, dropped...)
	}
	s.publishTasks(ctx, tasks)
}

func (s *BatchService) buildRequest(in SubmitInput) (value_objects.Request, error) {
	code, err := shared_vo.NewStockCode(in.Code, shared_vo.Market(in.Market))
	if err != nil {
		return value_objects.Request{}, err
	}
	tradeDate, err := shared_vo.NewTradeDate(in.TradeDate)
	if err != nil {
		return value_objects.Request{}, err
	}
	return value_objects.NewRequest(code, tradeDate, value_objects.Depth(in.Depth), in.Analysts, in.LLMModel)
}

func (s *BatchService) release(ctx context.Context, userID uint64, taskIDs ...string) {
	_, _ = s.guard.Release(ctx, userID, taskIDs...)
}

func (s *BatchService) publishBatch(ctx context.Context, b *entities.Batch) {
	if evts := b.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}

// publishTasks 把一批聚合的事件攒成一次发布，避免批量提交时 N 次投递。
func (s *BatchService) publishTasks(ctx context.Context, tasks []*entities.Task) {
	all := make([]domain_event.DomainEvent, 0, len(tasks))
	for _, t := range tasks {
		all = append(all, t.GetAllPendingEvents()...)
	}
	if len(all) > 0 {
		_ = s.publisher.Publish(ctx, all...)
	}
}
