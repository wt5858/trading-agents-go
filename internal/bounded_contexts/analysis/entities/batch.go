package entities

import (
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// MaxBatchSize 是单批次的子任务上限。
//
// 它是业务不变式而非配置：一次批量提交会同步占用同等数量的并发额度与队列深度，
// 上限过高会让单个用户挤满全局 worker。定在这里，任何入口都绕不过去。
//
// 它同时约束了 SettledTaskIDs 的规模——把「已结算 ID 集合」整份存进聚合
// 之所以可行，正是因为这个上限把集合钉死在百级。
const MaxBatchSize = 100

// SettleOutcome 是一次子任务结算的结果。
type SettleOutcome string

const (
	SettleCompleted SettleOutcome = "completed"
	SettleFailed    SettleOutcome = "failed"
)

// Batch 是批量分析的聚合根：有身份（ID）、有状态机（结算计数推进到 Done）。
//
// # 只按 ID 引用子任务
//
// 它不持有 Task 实体，只持有子任务 ID 列表。两个聚合各自独立落库与并发更新，
// 把 Task 塞进 Batch 会让每次子任务推进都要加载整个批次，也会立刻制造出
// 「一个事务同时写两个聚合根」这种跨聚合事务。
//
// # 结算靠集合而不是靠自增
//
// SettledTaskIDs 记录「已经计过数的子任务 ID」。Settle 先查重再计数，
// 因此重复投递（可见性超时回收会让同一个任务被执行两次）不会把计数推过头。
// 这是整条批量链路的幂等基石：结算是**集合的并集**，天然可重放。
//
// # 乐观锁
//
// Version 让「加载 - 结算 - 写回」这一串在并发下不会丢更新：
// 两个 worker 同时结算同一批次的两个子任务时，后写的那一次 UPDATE 命中 0 行，
// 由 domain_services/ 重新加载后重试。不能改成 SQL 端 `completed = completed + 1`：
// 那样就绕过了聚合，也就绕过了上面的查重。
type Batch struct {
	domain_event.EventRecorder

	ID             string
	UserID         uint64
	TaskIDs        []string
	SettledTaskIDs []string
	Total          int
	Completed      int
	Failed         int
	// Percent 是派生量（已结算数 ÷ 总数 × 100），随聚合一起持久化。
	// 读路径直接读这个字段，不再重算——理由同 value_objects.Progress。
	Percent   decimal.Decimal
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewBatch 创建批次。子任务集合在创建后不再变化。
func NewBatch(id string, userID uint64, taskIDs []string) (*Batch, error) {
	if id == "" {
		return nil, custom_errors.Invalid("批次 ID 不能为空")
	}
	if userID == 0 {
		return nil, custom_errors.Invalid("批次必须归属于一个用户")
	}
	if len(taskIDs) == 0 {
		return nil, custom_errors.Invalid("批量分析至少需要一只股票")
	}
	if len(taskIDs) > MaxBatchSize {
		return nil, custom_errors.Invalid("单批次最多 %d 只股票，当前 %d 只", MaxBatchSize, len(taskIDs))
	}

	now := time.Now()
	b := &Batch{
		ID:        id,
		UserID:    userID,
		TaskIDs:   append([]string(nil), taskIDs...),
		Total:     len(taskIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	b.AddDomainEvent(domain_events.NewOnBatchSubmitted(id, userID, b.TaskIDs))
	return b, nil
}

// Settle 登记一个子任务的结局，返回 true 表示这次调用真的改变了计数。
//
// 三重防线让它可以被无限次重放：
//   - 不属于本批次的 ID 直接忽略（迟到的、串批次的事件）；
//   - 已结算过的 ID 直接忽略（重投递、事件重发）；
//   - 结算数已达总数时不再接受（防止任何遗漏情况下计数溢出）。
//
// 因此调用方不需要「恰好一次」的投递保证，只需要「至少一次」——
// 这正是可以用领域事件而不是分布式事务来串两个聚合的前提。
func (b *Batch) Settle(taskID string, outcome SettleOutcome) bool {
	if taskID == "" || !b.contains(b.TaskIDs, taskID) {
		return false
	}
	if b.contains(b.SettledTaskIDs, taskID) {
		return false
	}
	if b.Done() {
		return false
	}

	b.SettledTaskIDs = append(b.SettledTaskIDs, taskID)
	if outcome == SettleCompleted {
		b.Completed++
	} else {
		b.Failed++
	}
	b.recomputed()

	if b.Done() {
		b.AddDomainEvent(domain_events.NewOnBatchCompleted(
			b.ID, b.UserID, b.Total, b.Completed, b.Failed))
	}
	return true
}

// Drop 把一批「根本没能进入系统」的子任务直接判失败。
//
// 用于批次已落库、但子任务写入或入队失败的补偿路径：与其留一个永远停在 99%
// 的批次，不如把差额如实记成失败，让批次立刻自洽并可结束。
// 返回真正被判失败的 ID，调用方据此归还并发名额。
func (b *Batch) Drop(taskIDs []string, reason string) []string {
	dropped := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		if b.Settle(id, SettleFailed) {
			dropped = append(dropped, id)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	b.AddDomainEvent(domain_events.NewOnBatchDegraded(b.ID, b.UserID, dropped, reason))
	return dropped
}

// PendingTaskIDs 返回尚未结算的子任务 ID，供对账程序使用。
func (b *Batch) PendingTaskIDs() []string {
	out := make([]string, 0, len(b.TaskIDs)-len(b.SettledTaskIDs))
	for _, id := range b.TaskIDs {
		if !b.contains(b.SettledTaskIDs, id) {
			out = append(out, id)
		}
	}
	return out
}

// Done 判断批次是否已全部结算。
//
// 用 >= 而不是 ==：即使某条补偿路径把计数推过了头，也不要让批次永远停在 99%。
func (b *Batch) Done() bool { return b.Completed+b.Failed >= b.Total }

func (b *Batch) OwnedBy(userID uint64) bool { return b.UserID == userID }

// recomputed 固化派生量。与 Progress 同理：百分比在写路径算一次并落库，
// 读路径不得再用 (completed+failed)/total 重算。
func (b *Batch) recomputed() {
	if b.Total <= 0 {
		b.Percent = decimal.Zero
	} else {
		b.Percent = decimalx.RoundPercent(
			decimal.NewFromInt(int64(b.Completed + b.Failed)).
				Div(decimal.NewFromInt(int64(b.Total))).
				Mul(decimal.NewFromInt(100)),
		)
	}
	b.UpdatedAt = time.Now()
}

// contains 线性查找。批次上限 100，线性扫描比维护一个需要序列化的 map 更划算，
// 也让聚合保持「纯数据 + 方法」的形状，便于直接从 DTO 重建。
func (b *Batch) contains(ids []string, target string) bool {
	return slices.Contains(ids, target)
}
