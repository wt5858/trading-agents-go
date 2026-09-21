package repositories

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SyncRunRepository 持久化 SyncRun 聚合。
//
// 它和 StockRepository 是同一上下文里的两个根，但事务从不跨越两者：
// 同步既写 Stock 又写 SyncRun，这是两次独立的原子写入，靠断点与统计对账，
// 而不是靠一个横跨几千个聚合的大事务——那种事务会把表锁到同步结束。
type SyncRunRepository struct {
	db *gorm.DB
}

func NewSyncRunRepository(db *gorm.DB) *SyncRunRepository {
	return &SyncRunRepository{db: db}
}

func (repo *SyncRunRepository) GetDb() *gorm.DB { return repo.db }

// TryStart 插入一次运行中的同步。
//
// 「同类型同市场同时只能有一个在跑」由 running_key 唯一索引保证，不由应用层查重保证：
// 先查「有没有在跑」再插入，两个并发触发会同时查到「没有」，然后双双插入——
// 经典的 TOCTOU。让数据库去拒绝第二个插入，检查与动作就合并成了一个原子操作。
func (repo *SyncRunRepository) TryStart(ctx context.Context, r *entities.SyncRun) error {
	dto := dtos.FromDomainSyncRun(r)
	if err := repo.db.WithContext(ctx).Create(dto).Error; err != nil {
		if isDuplicateKey(err) {
			return custom_errors.Conflict("%s / %s 已有同步任务正在运行", r.Kind.DisplayName(), r.Market.DisplayName())
		}
		return translateSQL(err, "创建同步记录")
	}
	return nil
}

// Save 更新同步运行的状态、断点与统计。
func (repo *SyncRunRepository) Save(ctx context.Context, r *entities.SyncRun) error {
	dto := dtos.FromDomainSyncRun(r)
	res := repo.db.WithContext(ctx).Model(&dtos.SyncRunDto{}).Where("id = ?", dto.ID).Updates(map[string]any{
		"status":       dto.Status,
		"running_key":  dto.RunningKey,
		"total":        dto.Total,
		"succeeded":    dto.Succeeded,
		"failed":       dto.Failed,
		"skipped":      dto.Skipped,
		"success_rate": dto.SuccessRate,
		"cursor":       dto.Cursor,
		"finished_at":  dto.FinishedAt,
		"duration_ms":  dto.DurationMS,
		"error":        dto.Error,
		"updated_at":   dto.UpdatedAt,
	})
	if res.Error != nil {
		return translateSQL(res.Error, "更新同步记录")
	}
	if res.RowsAffected == 0 {
		var n int64
		if err := repo.db.WithContext(ctx).Model(&dtos.SyncRunDto{}).Where("id = ?", dto.ID).Count(&n).Error; err != nil {
			return translateSQL(err, "更新同步记录")
		}
		if n == 0 {
			return custom_errors.NotFound("同步记录不存在: %s", dto.ID)
		}
	}
	return nil
}

func (repo *SyncRunRepository) FindByID(ctx context.Context, id string) (*entities.SyncRun, error) {
	var dto dtos.SyncRunDto
	if err := repo.db.WithContext(ctx).Where("id = ?", id).First(&dto).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("同步记录不存在: %s", id)
		}
		return nil, translateSQL(err, "查询同步记录")
	}
	return dto.ToDomain(), nil
}

// LatestOf 返回某类型某市场最近一次运行，运维面板与续传都用它。
func (repo *SyncRunRepository) LatestOf(ctx context.Context, kind value_objects.SyncKind, market shared_vo.Market) (*entities.SyncRun, error) {
	var dto dtos.SyncRunDto
	err := repo.db.WithContext(ctx).
		Where("kind = ? AND market = ?", kind.String(), market.String()).
		Order("started_at DESC").First(&dto).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("尚无 %s / %s 的同步记录", kind, market)
		}
		return nil, translateSQL(err, "查询最近同步记录")
	}
	return dto.ToDomain(), nil
}

// List 分页查询同步历史，kind / market 为空表示不过滤。
func (repo *SyncRunRepository) List(
	ctx context.Context,
	kind value_objects.SyncKind,
	market shared_vo.Market,
	page shared_vo.Page,
) ([]*entities.SyncRun, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.SyncRunDto{})
	if kind != "" {
		q = q.Where("kind = ?", kind.String())
	}
	if market != "" {
		q = q.Where("market = ?", market.String())
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translateSQL(err, "统计同步记录")
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*dtos.SyncRunDto
	if err := q.Order("started_at DESC").Offset(page.Offset()).Limit(page.Limit()).Find(&rows).Error; err != nil {
		return nil, 0, translateSQL(err, "查询同步历史")
	}
	return dtos.ToDomainSyncRuns(rows), total, nil
}

// MarkStaleAsFailed 把「卡在运行中」的记录批量判失败，返回清理数量。
//
// worker 被 kill -9 时不会有人去写终态，那条记录会永远停在 running，
// 它的 running_key 也就永远占着唯一索引——后果是这类同步再也起不来。
// 启动时扫一遍是唯一的解法，条件写在 WHERE 里而不是先查再改，避免与正在跑的实例打架。
func (repo *SyncRunRepository) MarkStaleAsFailed(ctx context.Context, olderThan time.Time) (int64, error) {
	now := time.Now()
	res := repo.db.WithContext(ctx).Model(&dtos.SyncRunDto{}).
		Where("status = ? AND started_at < ?", value_objects.SyncRunning.String(), olderThan).
		Updates(map[string]any{
			"status":      value_objects.SyncFailed.String(),
			"running_key": nil, // 释放唯一索引占位，否则同类同步永远无法再启动
			"error":       "运行超时未更新，判定为进程异常退出",
			"finished_at": now,
			"updated_at":  now,
		})
	if res.Error != nil {
		return 0, translateSQL(res.Error, "清理僵死同步记录")
	}
	return res.RowsAffected, nil
}

// FindResumable 找出可续传的最近一次运行。
func (repo *SyncRunRepository) FindResumable(ctx context.Context, kind value_objects.SyncKind, market shared_vo.Market) (*entities.SyncRun, error) {
	run, err := repo.LatestOf(ctx, kind, market)
	if err != nil {
		return nil, err
	}
	if !run.Resumable() {
		return nil, custom_errors.NotFound("没有可续传的 %s / %s 同步", kind, market)
	}
	return run, nil
}
