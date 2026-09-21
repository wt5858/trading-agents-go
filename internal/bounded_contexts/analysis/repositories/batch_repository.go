package repositories

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// BatchRepository 持久化 Batch 聚合。它只碰 analysis_batches 一张表。
//
// 它与 TaskRepository 是两个独立的仓储，各自只对自己的聚合根负责，
// 也各自只在自己的一条语句里保证原子性。本层没有任何方法会同时写两张表——
// 「不跨聚合共享事务」是靠「压根没有那样的方法」来保证的，而不是靠纪律。
type BatchRepository struct {
	db *gorm.DB
}

func NewBatchRepository(db *gorm.DB) *BatchRepository {
	return &BatchRepository{db: db}
}

func (repo *BatchRepository) GetDb() *gorm.DB { return repo.db }

// Create 首次落库批次。
//
// ON CONFLICT DO NOTHING 让它幂等：批量提交在网络抖动后被整体重试时，
// 同一个 batch_id 再写一次是无害的 no-op，而不是一个必须被调用方特判的冲突。
// 这条性质是「批次先落库、子任务后落库」这个顺序能安全重试的前提。
func (repo *BatchRepository) Create(ctx context.Context, b *entities.Batch) error {
	err := repo.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		Create(dtos.FromDomainBatch(b)).Error
	if err != nil {
		return translatef(err, "分析批次(id=%s)", b.ID)
	}
	return nil
}

// Save 以乐观锁写回批次的结算状态。
//
// # 为什么是乐观锁而不是 `completed = completed + 1`
//
// SQL 端自增确实原子，但它会绕过 Batch 聚合里的「同一个子任务只计一次」查重。
// 可见性超时重投会让同一个子任务结算两次，自增版本的计数会直接多算一个，
// 批次在还有任务在跑的时候就被判完成。
//
// 所以结算必须发生在聚合内部（读取 - Settle - 写回），而这一串要在并发下正确，
// 就需要把「我读到的版本仍然是当前版本」写进 WHERE。检查与写入因此是一个原子操作，
// 不存在先 SELECT 版本再 UPDATE 的那个竞态窗口。命中 0 行 = 有人抢先写过，
// 领域服务重新加载后重试即可（Settle 幂等，重放安全）。
func (repo *BatchRepository) Save(ctx context.Context, b *entities.Batch) error {
	dto := dtos.FromDomainBatch(b)
	res := repo.db.WithContext(ctx).Model(&dtos.BatchDto{}).
		Where("id = ? AND version = ?", dto.ID, dto.Version).
		Updates(map[string]any{
			"settled_task_ids": dto.SettledTaskIDs,
			"completed":        dto.Completed,
			"failed":           dto.Failed,
			"percent":          dto.Percent,
			"version":          dto.Version + 1,
			"updated_at":       dto.UpdatedAt,
		})
	if res.Error != nil {
		return translatef(res.Error, "分析批次(id=%s)", dto.ID)
	}
	if res.RowsAffected == 0 {
		var n int64
		if err := repo.db.WithContext(ctx).Model(&dtos.BatchDto{}).
			Where("id = ?", dto.ID).Count(&n).Error; err != nil {
			return translatef(err, "分析批次(id=%s)", dto.ID)
		}
		if n == 0 {
			return custom_errors.NotFound("分析批次(id=%s) 不存在", dto.ID)
		}
		return custom_errors.Conflict("分析批次(id=%s) 已被并发修改，请重试", dto.ID)
	}
	// 写成功后把版本推进到与库里一致，聚合可以继续被同一次调用复用。
	b.Version = dto.Version + 1
	return nil
}

func (repo *BatchRepository) FindByID(ctx context.Context, id string) (*entities.Batch, error) {
	var dto dtos.BatchDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		return nil, translatef(err, "分析批次(id=%s)", id)
	}
	return dto.ToDomain(), nil
}

// ListStale 取「尚未结算完、且已经安静了一段时间」的批次，供对账巡检使用。
//
// grace 的意义：刚刚提交的批次本来就处于未结算状态，立刻去对账只会和正常流程打架。
// 只捞 updated_at 早于 now-grace 的行，剩下的交给正常路径。
//
// limit 防止一次扫出上万条：对账是周期性任务，分批啃完即可。
func (repo *BatchRepository) ListStale(ctx context.Context, grace time.Duration, limit int) ([]*entities.Batch, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []dtos.BatchDto
	err := repo.db.WithContext(ctx).Model(&dtos.BatchDto{}).
		Where("completed + failed < total AND updated_at < ?", time.Now().Add(-grace)).
		Order("updated_at ASC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, translate(err, "待对账批次列表")
	}
	return dtos.ToDomainBatches(rows), nil
}

// ListByUser 走 idx_batches_user(user_id, created_at)，排序直接吃索引。
func (repo *BatchRepository) ListByUser(ctx context.Context, userID uint64, page shared_vo.Page) ([]*entities.Batch, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.BatchDto{}).
		Where("user_id = ?", userID).Session(&gorm.Session{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 批次列表", userID)
	}
	if total == 0 {
		return []*entities.Batch{}, 0, nil
	}

	var rows []dtos.BatchDto
	err := q.Order("created_at DESC, id DESC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error
	if err != nil {
		return nil, 0, translatef(err, "用户(id=%d) 批次列表", userID)
	}
	return dtos.ToDomainBatches(rows), total, nil
}
