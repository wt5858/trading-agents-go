package repositories

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// DefaultClaimLimit 是单轮 sweep 最多抢占的任务数。
//
// 它同时是两件事的闸门：一是本轮最多发出多少条 CAS 语句（见 ClaimDue 的注释），
// 二是本轮最多有多少个任务会被交给运行器。没有上限的话，一次长时间停机后的
// 首轮 sweep 会把积压的全部任务一次性拉起来，把停机的影响放大成一次自伤式的峰值。
const DefaultClaimLimit = 50

// ScheduledJobRepository 持久化 ScheduledJob 聚合。它只碰 scheduled_jobs 一张表。
//
// 这里是具体 struct 而不是接口：团队约定仓储直接注入实现，
// 为每个仓储再定义一个只有一个实现的接口，只会多一层无人受益的间接。
type ScheduledJobRepository struct {
	db *gorm.DB
}

func NewScheduledJobRepository(db *gorm.DB) *ScheduledJobRepository {
	return &ScheduledJobRepository{db: db}
}

func (repo *ScheduledJobRepository) GetDb() *gorm.DB { return repo.db }

// ---------------------------------------------------------------------------
// 分布式抢占
// ---------------------------------------------------------------------------

// ClaimDue 抢占到期的任务，只返回**本副本真正抢到**的那些。
//
// ===========================================================================
// 这是整个上下文里唯一真正难的东西，值得完整解释
// ===========================================================================
//
// # 问题
//
// worker 会部署 N 个副本，每个副本都跑一份一模一样的调度循环，每个 tick 都问
// 「现在有哪些任务到期了」。三个副本会同时看到同一条到期的任务，于是同一次触发
// 被执行三遍。对于「同步行情」这种幂等任务，代价是三倍的上游调用；
// 对于「按计划发起分析」这种非幂等任务，代价是用户收到三份重复报告、
// 被扣三份并发额度。这不是优化问题，是正确性问题。
//
// # 为什么不能先查再改（check-then-act）
//
// 最自然的写法是：SELECT 出到期任务 -> 内存里判断「它还没被别人拿走」-> UPDATE。
// 这在多副本下必然失效，因为三个副本的 SELECT 会在同一个瞬间**全部返回同一行**，
// 三个副本的内存判断也**全部成立**。这是典型的 TOCTOU：检查和动作之间有窗口，
// 而窗口里发生的事情检查者看不见。加日志、加重试、加随机 sleep 都只能让它更罕见，
// 不能让它消失——而更罕见的竞态只意味着更难排查。
//
// # 解法：把「检查」写成 UPDATE 的 WHERE 谓词
//
// 这正是 identity.UserRepository.Deactivate 守「至少保留一个启用管理员」用的同一招，
// 换到调度场景上：
//
//	UPDATE scheduled_jobs
//	   SET next_run_at = <新的下次触发时刻>, updated_at = ?
//	 WHERE id = ? AND next_run_at = <我 SELECT 时看到的那个值>
//
// next_run_at 在这里身兼两职：它既是业务字段，也是这一行的**乐观锁版本号**。
// 数据库对同一行的 UPDATE 是串行的，因此三个副本发出的三条语句必然排队执行：
//   - 第一条：WHERE 里的 next_run_at 仍然等于旧值，命中 1 行，同时把它改掉。
//   - 第二、三条：next_run_at 已经不是旧值了，命中 0 行。
//
// 于是 `RowsAffected == 1` 就是「我赢得了这一次触发」的**充分必要证据**，
// 而且这个证据由数据库本身给出，不依赖任何应用层的假设。检查与动作合并成了
// 一个原子操作，TOCTOU 窗口从根本上不存在了。
//
// 这也是为什么 ClaimDue 必须是仓储的方法而不是领域服务的方法：
// 保证来自 SQL 语句本身，只有能写 SQL 的这一层才能提供它。
//
// # 为什么是循环里发 N 条 UPDATE，而不是一条批量 UPDATE
//
// 「不要在循环里发 RPC」是本服务的通则，这里是一个必须说明理由的例外。
// 批量写法 `UPDATE ... WHERE id IN (...) AND ...` 只会返回一个**汇总的**
// RowsAffected，比如 3。但我们要回答的问题是「这 5 个里，哪 3 个是我赢的」——
// 汇总数回答不了，而猜错的代价就是漏跑或重跑。
// 每行一条语句是拿到逐行归属的唯一办法。
// 代价被 limit 钉死：单轮最多 DefaultClaimLimit 条语句，且只在真有任务到期时发生，
// 绝大多数 tick 的候选集是空的，一条 UPDATE 都不会发出。
//
// # 为什么不用 SELECT ... FOR UPDATE
//
// 行锁方案要开一个横跨 SELECT 与 UPDATE 的事务，锁会一直持有到事务提交。
// 一旦某个副本在持锁期间卡住（GC、网络抖动），其余副本会全部堵在锁上，
// 调度器整体停摆。CAS 方案完全无锁：输的副本立刻拿到 0 行并转身去做别的事。
//
// # 为什么不在这里开事务
//
// 每一条 CAS 语句自身就是原子的，这正是本方法全部保证的来源。
// 把 N 条 CAS 包进一个事务不会增加任何领域上的保证（N 个任务之间没有共同的不变式），
// 却会让锁持有时间从「一条语句」拉长到「整轮 sweep」，把无锁方案的好处全部退回去。
func (repo *ScheduledJobRepository) ClaimDue(ctx context.Context, now time.Time, limit int) ([]*entities.ScheduledJob, error) {
	if limit <= 0 {
		limit = DefaultClaimLimit
	}

	// 第一步：找候选。这一步**没有任何保证**，纯粹是为了把 CAS 的目标缩小到
	// 「可能到期」的少数几行，而不是对全表逐行试探。走 idx_jobs_due 覆盖过滤与排序。
	var rows []dtos.ScheduledJobDto
	err := repo.db.WithContext(ctx).
		Where("status = ? AND next_run_at <= ?", value_objects.JobStatusEnabled.String(), now).
		Order("next_run_at ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, translate(err, "到期定时任务")
	}
	if len(rows) == 0 {
		return nil, nil
	}

	won := make([]*entities.ScheduledJob, 0, len(rows))
	for i := range rows {
		job := rows[i].ToDomain()

		// expected 必须在聚合推进之前取，它就是 CAS 的期望值。
		expected := job.NextRunAt

		// 第二步：让聚合决定「下次是什么时候」。
		//
		// 推进逻辑属于领域层而不是 SQL：「错过的触发要不要补跑」是业务决策
		// （见 ClaimOccurrence 的注释），把它写成 SQL 表达式就等于把一条业务规则
		// 藏进了一条 UPDATE 里。这里也顺带挡掉了 cron 表达式坏掉的历史行。
		if _, err := job.ClaimOccurrence(now); err != nil {
			continue
		}

		// 第三步：CAS。RowsAffected == 1 是「我赢了这一次触发」的唯一证据。
		res := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
			Where("id = ? AND next_run_at = ?", job.ID, expected).
			Updates(map[string]any{
				"next_run_at": job.NextRunAt,
				"updated_at":  now,
			})
		if res.Error != nil {
			// 单条抢占失败不该让整轮 sweep 归零：已经赢下的任务必须被执行，
			// 否则它们的 next_run_at 已经推进了却没人跑，那一次触发就凭空消失了。
			// 返回已赢的部分，错误留给调用方记日志。
			return won, translatef(res.Error, "抢占定时任务(id=%s)", job.ID)
		}
		if res.RowsAffected != 1 {
			// 输了：别的副本先一步推进了 next_run_at。这是完全正常的路径，
			// 不是错误，也不该打日志——N 个副本里必然有 N-1 个走到这里。
			continue
		}
		won = append(won, job)
	}
	return won, nil
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

func (repo *ScheduledJobRepository) Create(ctx context.Context, j *entities.ScheduledJob) error {
	if err := repo.db.WithContext(ctx).Create(dtos.FromDomainJob(j)).Error; err != nil {
		return translatef(err, "定时任务(name=%s)", j.Name)
	}
	return nil
}

// Update 只写**定义与调度**相关的列。
//
// 刻意不写 status / last_* / 统计列：那些是执行路径的产出，由 SaveRunOutcome 负责。
// 两条写入路径各写各的列，是为了让「管理员在界面上改 cron」和
// 「worker 正好跑完一次任务」这两件同时发生的事不会互相覆盖——
// 如果两边都用一条全列 UPDATE，后写的那条会把对方的成果整段抹掉。
//
// kind / created_by / created_at 是任务的不可变契约，任何更新路径都不该碰。
func (repo *ScheduledJobRepository) Update(ctx context.Context, j *entities.ScheduledJob) error {
	dto := dtos.FromDomainJob(j)
	res := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
		Where("id = ?", dto.ID).
		Updates(map[string]any{
			"name":                     dto.Name,
			"cron_spec":                dto.CronSpec,
			"payload":                  dto.Payload,
			"next_run_at":              dto.NextRunAt,
			"timeout_seconds":          dto.TimeoutSeconds,
			"max_consecutive_failures": dto.MaxConsecutiveFailures,
			"updated_at":               dto.UpdatedAt,
		})
	if res.Error != nil {
		return translatef(res.Error, "定时任务(id=%s)", dto.ID)
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// 0 行是有歧义的：行可能不存在，也可能是值压根没变（重复提交同样的表单）。
	// 只在这一条分支上多查一次，让调用方拿到可区分的错误，正常路径仍然只有一条语句。
	return repo.assertExists(ctx, dto.ID)
}

// SaveRunOutcome 把一次执行的结局写回任务。
//
// 只写执行路径产出的列，不碰 next_run_at——它在 ClaimDue 里已经被推进过了，
// 这里再写一次等于把 CAS 的结果覆盖成一个可能已经过期的值。
// 也不碰定义列，理由见 Update。
//
// 不带条件谓词是安全的：能走到这里说明本副本赢下了这一次触发，
// 它是这一次执行结局的唯一书写者，不存在第二个竞争者。
func (repo *ScheduledJobRepository) SaveRunOutcome(ctx context.Context, j *entities.ScheduledJob) error {
	dto := dtos.FromDomainJob(j)
	res := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
		Where("id = ?", dto.ID).
		Updates(map[string]any{
			"status":               dto.Status,
			"last_run_at":          dto.LastRunAt,
			"last_status":          dto.LastStatus,
			"consecutive_failures": dto.ConsecutiveFailures,
			"total_runs":           dto.TotalRuns,
			"success_runs":         dto.SuccessRuns,
			// 成功率是写路径算好的派生量，跟着一起落库，读路径不再重算。
			"success_rate": dto.SuccessRate,
			"updated_at":   dto.UpdatedAt,
		})
	if res.Error != nil {
		return translatef(res.Error, "定时任务(id=%s) 执行结果", dto.ID)
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return repo.assertExists(ctx, dto.ID)
}

// Pause 暂停任务，把状态判定写成 WHERE 谓词。
//
// 没有「先加载再判断再保存」：那是 TOCTOU。两个管理员同时点暂停时，
// load-check-save 会让两次请求都通过内存判断、都写一次 updated_at，
// 第二次本该收到 Conflict 却拿到了成功。做成条件 UPDATE 之后，
// 第一个人赢，第二个人命中 0 行，拿到准确的「已处于暂停状态」。
//
// 同一条规则在 entities.ScheduledJob.Pause() 里也有一份：那是它的内存表达，
// 供单元测试与其他调用路径使用；并发下以这里的谓词为准。
func (repo *ScheduledJobRepository) Pause(ctx context.Context, id string, now time.Time) error {
	res := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
		Where("id = ? AND status = ?", id, value_objects.JobStatusEnabled.String()).
		Updates(map[string]any{
			"status":     value_objects.JobStatusPaused.String(),
			"updated_at": now,
		})
	if res.Error != nil {
		return translatef(res.Error, "定时任务(id=%s)", id)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	return repo.explainStatusMiss(ctx, id, value_objects.JobStatusPaused)
}

// Resume 恢复任务。
//
// 恢复必须重算 next_run_at，而重算需要 cron 表达式，所以这里不得不先读一次。
// 读与写之间的窗口由**谓词本身**封死：WHERE 同时钉住 status 和 cron_spec，
// 任何在窗口里改过状态或改过表达式的并发操作都会让这次 UPDATE 命中 0 行。
// 换句话说，我们不假设读到的值在写的时候仍然成立，而是让数据库去验证这个假设。
//
// consecutive_failures 一并清零：恢复一条被自动熔断的任务时，计数器还停在阈值上，
// 不清零的话下一次失败会立刻再次熔断，运维会以为恢复按钮失灵了。
func (repo *ScheduledJobRepository) Resume(ctx context.Context, id string, nextRunAt time.Time, expectedCronSpec string, now time.Time) error {
	res := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
		Where("id = ? AND status = ? AND cron_spec = ?",
			id, value_objects.JobStatusPaused.String(), expectedCronSpec).
		Updates(map[string]any{
			"status":               value_objects.JobStatusEnabled.String(),
			"consecutive_failures": 0,
			"next_run_at":          nextRunAt,
			"updated_at":           now,
		})
	if res.Error != nil {
		return translatef(res.Error, "定时任务(id=%s)", id)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	return repo.explainStatusMiss(ctx, id, value_objects.JobStatusEnabled)
}

func (repo *ScheduledJobRepository) Delete(ctx context.Context, id string) error {
	// 执行历史刻意不做级联删除：它是审计记录，「任务被删了」不代表
	// 「它从来没跑过」。历史由 PurgeOlderThan 按统一的保留期清理。
	res := repo.db.WithContext(ctx).Where("id = ?", id).Delete(&dtos.ScheduledJobDto{})
	if res.Error != nil {
		return translatef(res.Error, "定时任务(id=%s)", id)
	}
	if res.RowsAffected == 0 {
		return custom_errors.NotFound("定时任务(id=%s) 不存在", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (repo *ScheduledJobRepository) FindByID(ctx context.Context, id string) (*entities.ScheduledJob, error) {
	var dto dtos.ScheduledJobDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		return nil, translatef(err, "定时任务(id=%s)", id)
	}
	return dto.ToDomain(), nil
}

func (repo *ScheduledJobRepository) FindByName(ctx context.Context, name string) (*entities.ScheduledJob, error) {
	var dto dtos.ScheduledJobDto
	if err := repo.db.WithContext(ctx).Where("name = ?", name).First(&dto).Error; err != nil {
		return nil, translatef(err, "定时任务(name=%s)", name)
	}
	return dto.ToDomain(), nil
}

// List 按种类与状态筛选，零值表示不限。走 idx_jobs_kind_status。
//
// 排序用 next_run_at ASC, id ASC：管理界面最关心「接下来要跑什么」。
// 补一个 id 做 tie-break，否则同一秒触发的多条任务在翻页时会重复出现。
func (repo *ScheduledJobRepository) List(
	ctx context.Context,
	kind value_objects.JobKind,
	status value_objects.JobStatus,
	page shared_vo.Page,
) ([]*entities.ScheduledJob, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{})
	if !kind.IsZero() {
		q = q.Where("kind = ?", kind.String())
	}
	if !status.IsZero() {
		q = q.Where("status = ?", status.String())
	}
	// Session 固化条件，让 Count 与 Find 复用同一份 where 而不互相污染。
	q = q.Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translate(err, "定时任务列表")
	}
	if total == 0 {
		return []*entities.ScheduledJob{}, 0, nil
	}

	var rows []dtos.ScheduledJobDto
	err := q.Order("next_run_at ASC, id ASC").
		Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translate(err, "定时任务列表")
	}
	return dtos.ToDomainJobs(rows), total, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// assertExists 把「0 行」这个歧义结果收敛成一个确定的答案。
func (repo *ScheduledJobRepository) assertExists(ctx context.Context, id string) error {
	var n int64
	if err := repo.db.WithContext(ctx).Model(&dtos.ScheduledJobDto{}).
		Where("id = ?", id).Count(&n).Error; err != nil {
		return translatef(err, "定时任务(id=%s)", id)
	}
	if n == 0 {
		return custom_errors.NotFound("定时任务(id=%s) 不存在", id)
	}
	// 行在、但值没变：把它当成成功。重复提交同一份表单不该是一个错误。
	return nil
}

// explainStatusMiss 解释一次条件 UPDATE 为什么没命中。
//
// 这一次额外查询只发生在失败分支上，正常路径仍然只有一条语句。
// 值得为它多跑一次查询的理由是：「暂停失败了」对调用方毫无意义，
// 而「任务已处于暂停状态」和「任务不存在」是两种完全不同的处置。
func (repo *ScheduledJobRepository) explainStatusMiss(ctx context.Context, id string, target value_objects.JobStatus) error {
	var current dtos.ScheduledJobDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return custom_errors.NotFound("定时任务(id=%s) 不存在", id)
		}
		return translatef(err, "定时任务(id=%s)", id)
	}
	status := value_objects.JobStatus(current.Status)
	if status == target {
		return custom_errors.Conflict("任务已处于%s状态", status.DisplayName())
	}
	return custom_errors.Conflict("任务当前处于%s状态，不允许切换到%s",
		status.DisplayName(), target.DisplayName())
}
