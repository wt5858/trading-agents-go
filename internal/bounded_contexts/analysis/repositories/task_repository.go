package repositories

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// insertChunk 是批量插入的分片大小。
// 单条 INSERT 的参数个数受 max_allowed_packet 与占位符上限约束，
// 100 条 × 十几列远低于任何默认阈值，同时把 RPC 次数压到批次大小的 1/100。
const insertChunk = 100

// TaskRepository 持久化 Task 聚合。它只碰 analysis_tasks 一张表。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
type TaskRepository struct {
	db *gorm.DB
}

func NewTaskRepository(db *gorm.DB) *TaskRepository {
	return &TaskRepository{db: db}
}

func (repo *TaskRepository) GetDb() *gorm.DB { return repo.db }

// Save 首次落库单个任务聚合。
//
// 用 ON CONFLICT DO NOTHING 而不是裸 INSERT：提交路径可能被客户端重试，
// 同一个任务 ID 再写一次应当是无害的 no-op，而不是一个需要调用方分辨的
// AlreadyExists 错误。「重复提交等于什么都没发生」是这条路径唯一说得通的语义。
func (repo *TaskRepository) Save(ctx context.Context, t *entities.Task) error {
	err := repo.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		Create(dtos.FromDomainTask(t)).Error
	if err != nil {
		return translatef(err, "分析任务(id=%s)", t.ID)
	}
	return nil
}

// SaveAll 批量落库一组任务聚合。
//
// # 为什么没有事务
//
// 这里写入的是 N 个**同一种**聚合根的独立实例，每一条 INSERT 自身就是原子的。
// 把它们包进一个事务不会带来任何领域上的保证（Task 之间没有互相依赖的不变式），
// 只会拉长锁持有时间。真正需要跨聚合协调的是 Batch，那由 domain_services/
// 用「各自落库 + 领域事件 + 幂等结算」来串，而不是共享事务——见 BatchService。
//
// # 为什么是幂等的
//
// ON CONFLICT DO NOTHING + 调用方生成的稳定 ID，意味着部分失败后原样重试即可：
// 已写入的行被跳过，缺失的行被补上，最终收敛到同一个结果。
// 这正是「批次已存在但子任务写了一半」可恢复的技术前提。
//
// 分片多值 INSERT 而不是循环 Save：避免在调用方制造 N 次 RPC。
func (repo *TaskRepository) SaveAll(ctx context.Context, tasks []*entities.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	err := repo.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		CreateInBatches(dtos.FromDomainTasks(tasks), insertChunk).Error
	if err != nil {
		return translate(err, "分析任务批量写入")
	}
	return nil
}

// ClaimTask 认领一个排队中的任务，把它从 queued 推进到 running。
//
// 返回认领到的聚合；返回 (nil, nil) 表示没抢到——任务不存在、已被别的消费者
// 接手、或早已进入终态。这三种情况对调用方是同一件事：这条消息没有活要干。
//
// # 为什么必须是一条带谓词的 UPDATE
//
// 投递是至少一次的。信道断开、broker 重启、消费超时，都会让同一个任务在前一个
// 消费者还在跑的时候被重新投递。「先查一下是不是 queued，再改成 running」挡不住
// 它：两次查询都会返回 queued，两个消费者都会开跑，同一次分析的 LLM 账单付两遍。
//
// 把判断写进 WHERE 之后，检查与写入成为同一个原子操作，行锁保证只有一个副本命中
// 1 行。输的那个拿到 0 行，确认消息走人。这与 scheduling 上下文的 ClaimQueued
// 是同一套协议，不另立一种。
//
// # 状态迁移仍然由聚合完成
//
// 本方法不自己拼 status='running'：Attempts 怎么加、StartedAt 是否覆盖、进度文案
// 换成什么、要抛哪条领域事件，全是 Task.Start() 的职责。这里只负责让那次迁移的
// 落库变成有条件的——仓储提供原子性，规则仍在实体。
func (repo *TaskRepository) ClaimTask(ctx context.Context, taskID string) (*entities.Task, error) {
	queued := value_objects.StatusQueued.String()

	var row dtos.TaskDto
	err := repo.db.WithContext(ctx).
		Where("id = ? AND status = ?", taskID, queued).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, translatef(err, "分析任务(id=%s)", taskID)
	}

	task := row.ToDomain()
	if err := task.Start(); err != nil {
		// 选出来的行状态就是 queued，Start 没有理由拒绝它。真走到这里说明库里的
		// status 越过了 Status 的取值域，那是数据问题，不是竞态。
		return nil, err
	}

	dto := dtos.FromDomainTask(task)
	res := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("id = ? AND status = ?", taskID, queued).
		Updates(map[string]any{
			"status":           dto.Status,
			"attempts":         dto.Attempts,
			"started_at":       dto.StartedAt,
			"progress":         dto.Progress,
			"state_changed_at": dto.StateChangedAt,
		})
	if res.Error != nil {
		return nil, translatef(res.Error, "分析任务(id=%s)", taskID)
	}
	if res.RowsAffected != 1 {
		// 输了。重复投递下这是常态而不是错误，也不打日志——
		// 否则每条重投消息都会在日志里留下一条「无事发生」。
		return nil, nil
	}
	return task, nil
}

// ListStale 捞出卡住的任务，供停滞巡检处置。走 idx_tasks_stale。
//
// 两种卡法，因此两个阈值——它们问的是同一件事（这一行维持当前状态多久了），
// 但成因和处置完全不同，混成一个阈值就没法给出正确的处置：
//
//	queued 卡太久   消息没送到（提交后发布失败、broker 丢了、消费者集体不在）。
//	                任务本身没问题，补一条消息即可。
//	running 卡太久  认领成功之后消费者死了。没有人会再给它写结局，
//	                必须判它失败，再由重试策略决定要不要重来。
//
// 只返回聚合，不做任何判断：捞出来之后怎么处置是领域决策，归 domain_services/。
//
// # 为什么这个巡检是必须的
//
// 认领把「同一个任务只能有一个消费者开工」做成了 UPDATE 的谓词，代价是赢家崩掉之后
// 那一行会永远停在 running——谓词挡住了所有后来者，包括本该接手的那个。
// 原先这由 Redis 的可见性超时 + 心跳续约兜底，那套机制随队列一起下线。
// 没有这个巡检，卡住的任务表现是**安静的**：状态正常、没有错误日志，
// 只是它再也不会动，而用户看到的是一个永远停在「分析中」的进度条。
func (repo *TaskRepository) ListStale(
	ctx context.Context, queuedBefore, runningBefore time.Time, limit int,
) ([]*entities.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []dtos.TaskDto
	err := repo.db.WithContext(ctx).
		Where("(status = ? AND state_changed_at < ?) OR (status = ? AND state_changed_at < ?)",
			value_objects.StatusQueued.String(), queuedBefore,
			value_objects.StatusRunning.String(), runningBefore).
		// 先卡住的先处置：积压时这保证最老的任务不会被后来者反复插队。
		Order("state_changed_at ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, translate(err, "查询卡住的分析任务")
	}
	return dtos.ToDomainTasks(rows), nil
}

// Update 只写生命周期相关的列，并把「终局不可改写」做成 WHERE 谓词。
//
// # 不变式即谓词
//
// `status NOT IN ('completed','canceled')` 不是防御性编程，而是把不变式交给数据库。
// 重投递会让同一个任务被两个消费者同时执行；两者都在内存里看到 running，
// 都判定自己可以收尾。先读一次确认状态再写（check-then-act）挡不住这个竞态——
// 两次读都会返回 running。写成 UPDATE 的条件后，检查与写入成为同一个原子操作，
// 第一个收尾者获胜，第二个命中 0 行并拿到 Conflict，不会覆盖已经产出的结果。
//
// failed 刻意不在谓词里：它是一次未竟的尝试，Requeue 必须能把它改回 queued。
//
// request / user_id / created_at 是任务的不可变契约，worker 重试时不该有机会改动它们；
// 冗余列 symbol/market/trade_date 由 request 派生，同理不更新。
func (repo *TaskRepository) Update(ctx context.Context, t *entities.Task) error {
	dto := dtos.FromDomainTask(t)
	res := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("id = ? AND status NOT IN ?", dto.ID, value_objects.FinalStatuses()).
		Updates(map[string]any{
			"status":           dto.Status,
			"batch_id":         dto.BatchID,
			"progress":         dto.Progress,
			"result":           dto.Result,
			"error":            dto.Error,
			"attempts":         dto.Attempts,
			"started_at":       dto.StartedAt,
			"finished_at":      dto.FinishedAt,
			"state_changed_at": dto.StateChangedAt,
		})
	if res.Error != nil {
		return translatef(res.Error, "分析任务(id=%s)", dto.ID)
	}
	if res.RowsAffected > 0 {
		return nil
	}

	// 0 行有三种可能：行不存在、任务已进入终局、或者值压根没变
	// （进度心跳经常写入完全相同的快照）。只在这一条分支上多查一次，
	// 让调用方拿到可区分的错误，而正常路径仍然只有一条语句。
	var current dtos.TaskDto
	if err := repo.db.WithContext(ctx).Where("id = ?", dto.ID).First(&current).Error; err != nil {
		return translatef(err, "分析任务(id=%s)", dto.ID)
	}
	if value_objects.Status(current.Status).Final() && current.Status != dto.Status {
		return custom_errors.Conflict("分析任务(id=%s) 已处于终局状态 %s，拒绝改写",
			dto.ID, value_objects.Status(current.Status).DisplayName())
	}
	return nil
}

// FailAll 把一组任务的「失败」结局一次性写回。
//
// 领域决策仍然发生在各自的聚合里——调用方先对每个 Task 调 Fail()，拿到事件；
// 这里只负责把同一个结局批量落盘。N 次 UPDATE 压成一条带 IN 的语句，
// 避免在调用方制造 N 次 RPC。
//
// WHERE 依旧带终局谓词：这条路径只用于补偿，绝不能覆盖掉某个已经跑完的任务。
func (repo *TaskRepository) FailAll(ctx context.Context, tasks []*entities.Task, reason string) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	now := time.Now()
	err := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("id IN ? AND status NOT IN ?", ids, value_objects.FinalStatuses()).
		Updates(map[string]any{
			"status":      value_objects.StatusFailed.String(),
			"error":       reason,
			"finished_at": now,
			// 状态变了，state_changed_at 就必须跟着变。这条路径绕过了聚合直接写列，
			// 漏掉它会让这批任务在停滞巡检眼里"卡在 failed 很久"——虽然 failed 是终态、
			// 当前不会被捞出来，但那是巧合而不是保证，不值得赌下一次谓词调整。
			"state_changed_at": now,
		}).Error
	if err != nil {
		return translate(err, "分析任务批量置失败")
	}
	return nil
}

func (repo *TaskRepository) FindByID(ctx context.Context, id string) (*entities.Task, error) {
	var dto dtos.TaskDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		return nil, translatef(err, "分析任务(id=%s)", id)
	}
	return dto.ToDomain(), nil
}

// ListByUser 走 idx_tasks_user_status。status 传零值表示不限状态。
//
// 排序用 created_at DESC, id DESC：任务 ID 不保证单调，只按 created_at 排，
// 在同毫秒创建的批量任务上会出现翻页重复，补一个 id 做 tie-break。
func (repo *TaskRepository) ListByUser(ctx context.Context, userID uint64, status value_objects.Status, page shared_vo.Page) ([]*entities.Task, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).Where("user_id = ?", userID)
	if !status.IsZero() {
		q = q.Where("status = ?", status.String())
	}
	// Session 固化条件，让 Count 与 Find 复用同一份 where 而不互相污染。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 任务列表", userID)
	}
	if total == 0 {
		return []*entities.Task{}, 0, nil
	}

	var rows []dtos.TaskDto
	err := q.Order("created_at DESC, id DESC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 任务列表", userID)
	}
	return dtos.ToDomainTasks(rows), total, nil
}

// ListByBatch 走 idx_tasks_batch 反查，不读 analysis_batches.task_ids：
// 冗余的 ID 数组只用于保留提交顺序，任务实体永远以 analysis_tasks 表为准。
// 这里不分页，因为单批次上限由 Batch 聚合约束在百级。
func (repo *TaskRepository) ListByBatch(ctx context.Context, batchID string) ([]*entities.Task, error) {
	if batchID == "" {
		// 空 batch_id 是「非批量任务」的默认值，放行会把全库散任务捞出来。
		return nil, custom_errors.Invalid("批次 ID 不能为空")
	}
	var rows []dtos.TaskDto
	err := repo.db.WithContext(ctx).Where("batch_id = ?", batchID).
		Order("created_at ASC, id ASC").Find(&rows).Error
	if err != nil {
		return nil, translatef(err, "分析批次(id=%s) 的任务", batchID)
	}
	return dtos.ToDomainTasks(rows), nil
}

// ExistingIDsOfBatch 只取 ID 列，用于对账：批次声明了 N 个子任务，
// 实际落库了哪些。一条走覆盖索引的查询，不反序列化任何 JSON 列。
func (repo *TaskRepository) ExistingIDsOfBatch(ctx context.Context, batchID string) ([]string, error) {
	if batchID == "" {
		return nil, custom_errors.Invalid("批次 ID 不能为空")
	}
	var ids []string
	err := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("batch_id = ?", batchID).Pluck("id", &ids).Error
	if err != nil {
		return nil, translatef(err, "分析批次(id=%s) 的任务 ID", batchID)
	}
	return ids, nil
}

// CountRunningByUser 是并发配额的兜底校验（快路径在 Redis 的 ConcurrencyGuard）。
//
// 只数 running：queued 任务还没占用 worker 名额，把它算进去会让用户在
// 队列拥塞时永远提交失败。
func (repo *TaskRepository) CountRunningByUser(ctx context.Context, userID uint64) (int64, error) {
	var total int64
	err := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("user_id = ? AND status = ?", userID, value_objects.StatusRunning.String()).
		Count(&total).Error
	if err != nil {
		return 0, translatef(err, "用户(id=%d) 运行中任务数", userID)
	}
	return total, nil
}

func (repo *TaskRepository) CountRunning(ctx context.Context) (int64, error) {
	var total int64
	err := repo.db.WithContext(ctx).Model(&dtos.TaskDto{}).
		Where("status = ?", value_objects.StatusRunning.String()).
		Count(&total).Error
	if err != nil {
		return 0, translate(err, "全局运行中任务数")
	}
	return total, nil
}
