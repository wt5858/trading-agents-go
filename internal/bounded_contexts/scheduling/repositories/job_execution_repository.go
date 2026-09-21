package repositories

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// purgeChunk 是保留期清理的单批删除行数。
//
// 分批而不是一条 `DELETE WHERE started_at < ?` 删干净：一次删掉几十万行会持有
// 大量行锁、撑爆 binlog 事务、并在主从架构上制造可观的复制延迟。
// 分批之后每条语句都是短事务，清理作业可以和调度器并行跑而互不打扰。
const purgeChunk = 1000

// JobExecutionRepository 持久化 JobExecution 聚合。它只碰 job_executions 一张表。
//
// # 为什么 JobExecution 值得拥有自己的仓储
//
// 因为它是聚合根，而不是 ScheduledJob 的子实体。完整论证在
// entities/job_execution.go 的类型注释里，一句话版本：它的规模（单任务年产数十万行）
// 和它独立的生命周期（按保留期清理）都要求它能被独立读写，
// 而作为子实体，追加一行审计记录就得先把整条历史加载进内存。
//
// 「只有聚合根才有仓储」这条规则在这里没有被打破——恰恰相反，
// 正是因为它确实是根，它才配有一个仓储。
type JobExecutionRepository struct {
	db *gorm.DB
}

func NewJobExecutionRepository(db *gorm.DB) *JobExecutionRepository {
	return &JobExecutionRepository{db: db}
}

func (repo *JobExecutionRepository) GetDb() *gorm.DB { return repo.db }

// ---------------------------------------------------------------------------
// 写入：三条路径，对应生命周期的三次推进
// ---------------------------------------------------------------------------
//
//	Enqueue     无 -> queued    调度器抢到触发，写下欠条
//	ClaimQueued queued -> running 消费端认领这次执行
//	Finish      running -> 终态  执行收尾
//
// 后两条都是带谓词的 UPDATE。谓词不是防御性编程，它就是并发正确性本身：
// 「这条记录还欠着吗」这个检查若与「把它标成执行中」分成两步，
// 两个消费者会同时通过检查，然后把同一次触发跑两遍。

// Enqueue 写下一条待执行记录。冲突即视为已经有人写过，不算错误。
//
// # 为什么冲突要当成成功
//
// 重复入队的来源是正常运维动作，不是异常：调度器在「CAS 抢到触发」与
// 「写下这条记录」之间被杀掉再重启，恢复巡检会重新入队同一次触发。
// 此时正确的行为是「已经有了，很好」，而不是抛一个需要调用方分辨的 AlreadyExists——
// 后者会逼着每个调用点都写一段 if errors.Is(AlreadyExists) { 当成功处理 }，
// 而那段代码总有一处会被忘记。
//
// 返回值 created 告诉调用方这条记录是不是本次新建的。它只用来决定
// **要不要发消息**：不是自己建的就说明消息已经有人发过了，再发一条只是重复投递。
// 即便判断错了也不会出问题（消费端的认领是幂等的），但没必要制造无谓的消息。
//
// 冲突的判定依赖 uk_exec_occurrence(job_id, scheduled_for, attempt) 这个唯一索引，
// 而不是主键——主键是应用生成的随机 ID，两次入队会生成两个不同的 ID，
// 靠主键根本撞不上。真正标识「同一次尝试」的是那三列。
func (repo *JobExecutionRepository) Enqueue(ctx context.Context, e *entities.JobExecution) (created bool, err error) {
	res := repo.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(dtos.FromDomainExecution(e))
	if res.Error != nil {
		return false, translatef(res.Error, "任务执行记录(job=%s)", e.JobID)
	}
	return res.RowsAffected == 1, nil
}

// ClaimQueued 认领某一次触发当前欠着的那条记录，把它从 queued 推进到 running。
//
// ===========================================================================
// 这是整个消息驱动流程里唯一真正难的东西
// ===========================================================================
//
// # 问题
//
// 消息队列的投递是**至少一次**的。同一条「这次触发该跑了」的消息会因为
// 重投、重连补投、消费者被杀而到达不止一次，而消费者有多个副本。
// 若不加处理，一次触发就会被跑好几遍——对行情同步而言是几倍的上游调用，
// 对「按计划发起分析」而言是用户收到几份重复报告、被扣几份并发额度。
//
// # 为什么不能先查再改
//
// 最自然的写法是：查出这条 queued 记录 -> 内存里判断「它还没被人拿走」-> 改成 running。
// 两个消费者的查询会在同一瞬间**都**返回这条记录，两边的内存判断也**都**成立。
// 这是典型的 TOCTOU。加日志、加重试、加随机 sleep 都只能让它更罕见而不能让它消失，
// 而更罕见的竞态只意味着更难排查。
//
// # 解法：把判断写进 UPDATE 的 WHERE
//
// 与本上下文 ClaimDue 用的是同一招。数据库对同一行的 UPDATE 是串行的，
// 因此两条语句必然排队：第一条看到 status 仍是 queued，命中 1 行并改掉它；
// 第二条看到 status 已经是 running，命中 0 行。
// `RowsAffected == 1` 于是成为「我赢得了这次执行」的充分必要证据，
// 而这个证据由数据库本身给出，不依赖任何应用层的假设。
//
// # 为什么先 SELECT 一次
//
// 只是为了拿到主键，把 CAS 的目标缩小到一行。这一步**不提供任何保证**，
// 保证全部来自第二步 WHERE 里的 status 谓词——它会重新验证那个假设。
// 返回 (nil, nil) 的含义是「这次触发不欠任何东西了」：可能已经被别的副本认领，
// 可能早已跑完。两种情况对调用方是同一件事——确认消息然后走人。
func (repo *JobExecutionRepository) ClaimQueued(
	ctx context.Context, jobID string, scheduledFor time.Time,
) (*entities.JobExecution, error) {
	queued := value_objects.ExecutionStatusQueued.String()

	var row dtos.JobExecutionDto
	err := repo.db.WithContext(ctx).
		Where("job_id = ? AND scheduled_for = ? AND status = ?", jobID, scheduledFor, queued).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, translatef(err, "定时任务(id=%s) 的待执行记录", jobID)
	}

	startedAt := time.Now()
	res := repo.db.WithContext(ctx).Model(&dtos.JobExecutionDto{}).
		Where("id = ? AND status = ?", row.ID, queued).
		Updates(map[string]any{
			"status":     value_objects.ExecutionStatusRunning.String(),
			"started_at": startedAt,
		})
	if res.Error != nil {
		return nil, translatef(res.Error, "任务执行记录(id=%s)", row.ID)
	}
	if res.RowsAffected != 1 {
		// 输了。这是完全正常的路径，不是错误，也不该打日志——
		// 重复投递是消息队列的常态，N 次投递里必然有 N-1 次走到这里。
		return nil, nil
	}

	// 用已读到的行加上刚写进去的两个值拼出聚合，不再回查一次：
	// 回查既多一次往返，也会在两次读之间引入一个新的、毫无必要的窗口。
	row.Status = value_objects.ExecutionStatusRunning.String()
	row.StartedAt = startedAt
	return row.ToDomain(), nil
}

// Finish 给一条执行中的记录写下结局。
//
// WHERE 里钉住 status = running，理由和 ClaimQueued 一样，只是防的是另一件事：
// 恢复巡检可能已经把一条卡死的 running 记录判成失败，而原来的消费者随后
// 又活过来想写自己的结局。谓词让后到的那次写入命中 0 行，
// 「终态不可改写」这条不变式因此由数据库兜底，而不是只靠聚合里的内存判断。
//
// 命中 0 行不报错：调用方对此无能为力，结局已经有人写下了，
// 抛错只会让收尾路径多一段没有正确处置方式的错误分支。
func (repo *JobExecutionRepository) Finish(ctx context.Context, e *entities.JobExecution) error {
	res := repo.db.WithContext(ctx).Model(&dtos.JobExecutionDto{}).
		Where("id = ? AND status = ?", e.ID, value_objects.ExecutionStatusRunning.String()).
		Updates(map[string]any{
			"status":      e.Status.String(),
			"summary":     e.Summary,
			"item_count":  e.ItemCount,
			"error":       e.ErrMsg,
			"finished_at": e.FinishedAt,
			"duration_ms": e.DurationMs,
		})
	if res.Error != nil {
		return translatef(res.Error, "任务执行记录(id=%s)", e.ID)
	}
	return nil
}

// ListStale 找出卡住的执行记录，供恢复巡检使用。
//
// 两种卡法，原因完全不同，但都表现为「一条记录停在非终态很久了」：
//
//	queued 卡住   消息没能送到队列（发布失败，或进程在发布前被杀）
//	running 卡住  消费者在执行途中死了，没有人会再给它写结局
//
// 阈值由调用方分别给出：queued 只需要等一个「消息本该早就被消费掉」的时长，
// 而 running 必须等到超过任务自身的执行超时——一次行情同步跑十分钟是正常的，
// 用同一个短阈值去判它，会把正在好好干活的执行强行判死。
func (repo *JobExecutionRepository) ListStale(
	ctx context.Context, queuedBefore, runningBefore time.Time, limit int,
) ([]*entities.JobExecution, error) {
	if limit <= 0 {
		limit = DefaultClaimLimit
	}
	var rows []dtos.JobExecutionDto
	err := repo.db.WithContext(ctx).
		Where("(status = ? AND queued_at < ?) OR (status = ? AND started_at < ?)",
			value_objects.ExecutionStatusQueued.String(), queuedBefore,
			value_objects.ExecutionStatusRunning.String(), runningBefore).
		Order("queued_at ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, translate(err, "卡住的任务执行记录")
	}
	return dtos.ToDomainExecutions(rows), nil
}

func (repo *JobExecutionRepository) FindByID(ctx context.Context, id string) (*entities.JobExecution, error) {
	var dto dtos.JobExecutionDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		return nil, translatef(err, "任务执行记录(id=%s)", id)
	}
	return dto.ToDomain(), nil
}

// ListByJob 分页返回某任务的执行历史，走 idx_exec_job。
//
// 倒序：看历史永远是从最近一次看起。分页是强制的，不提供「全部拉回」的入口——
// 单条任务的历史轻易就是六位数行，一个不分页的接口迟早会被人调用一次然后打爆内存。
func (repo *JobExecutionRepository) ListByJob(
	ctx context.Context, jobID string, page shared_vo.Page,
) ([]*entities.JobExecution, int64, error) {
	if jobID == "" {
		// 空 job_id 会把全表捞出来，直接拦住。
		return nil, 0, custom_errors.Invalid("任务 ID 不能为空")
	}
	q := repo.db.WithContext(ctx).Model(&dtos.JobExecutionDto{}).
		Where("job_id = ?", jobID).
		Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "定时任务(id=%s) 执行历史", jobID)
	}
	if total == 0 {
		return []*entities.JobExecution{}, 0, nil
	}

	var rows []dtos.JobExecutionDto
	err := q.Order("started_at DESC, id DESC").
		Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "定时任务(id=%s) 执行历史", jobID)
	}
	return dtos.ToDomainExecutions(rows), total, nil
}

// PurgeOlderThan 按保留期清理执行历史，返回删除的行数。
//
// 分批循环删除，直到某一批不足 purgeChunk 为止。循环里的每条 DELETE 都是
// 独立的短事务，ctx 取消会让循环立刻停下并返回**已经删掉的行数**——
// 中途停止是完全安全的，因为删除是幂等的，下一轮清理会接着删剩下的。
//
// 这不违反「不在循环里发 RPC」：分批本身就是为了避免单条巨型语句，
// 而每一批都删掉 purgeChunk 行，RPC 次数与数据量之比是 1:1000，不是 1:1。
func (repo *JobExecutionRepository) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			// 清理是尽力而为的后台作业，停机时已删掉的部分照常有效。
			return deleted, nil
		}
		res := repo.db.WithContext(ctx).
			Where("started_at < ?", cutoff).
			Limit(purgeChunk).
			Delete(&dtos.JobExecutionDto{})
		if res.Error != nil {
			return deleted, translate(res.Error, "清理任务执行历史")
		}
		deleted += res.RowsAffected
		if res.RowsAffected < purgeChunk {
			return deleted, nil
		}
	}
}

// CountByJob 给出某任务的历史条数，供管理界面在不翻页的情况下展示总量。
func (repo *JobExecutionRepository) CountByJob(ctx context.Context, jobID string) (int64, error) {
	var n int64
	if err := repo.db.WithContext(ctx).Model(&dtos.JobExecutionDto{}).
		Where("job_id = ?", jobID).Count(&n).Error; err != nil {
		return 0, translatef(err, "定时任务(id=%s) 执行次数", jobID)
	}
	return n, nil
}
