package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// SyncRunDto 是 sync_runs 表的持久化对象。
type SyncRunDto struct {
	ID     string `gorm:"column:id;type:varchar(48);primaryKey"`
	Kind   string `gorm:"column:kind;type:varchar(24);not null;index:idx_sync_runs_kind_market,priority:1"`
	Market string `gorm:"column:market;type:varchar(8);not null;index:idx_sync_runs_kind_market,priority:2"`
	Status string `gorm:"column:status;type:varchar(16);not null;index:idx_sync_runs_status"`

	// RunningKey 让「同类型同市场只能有一个运行中实例」成为数据库约束。
	//
	// MySQL 没有部分索引，但唯一索引不约束 NULL：运行中写 "kind:market"，
	// 进入终态写 NULL，于是唯一索引天然只对运行中的行生效。
	// 这样并发重复触发会被插入时的 1062 直接挡掉——
	// 换成「先查有没有在跑、再插入」就是典型的 TOCTOU，两个请求都能查到「没有」。
	RunningKey *string `gorm:"column:running_key;type:varchar(40);uniqueIndex:uk_sync_runs_running"`

	Total       int             `gorm:"column:total;not null;default:0"`
	Succeeded   int             `gorm:"column:succeeded;not null;default:0"`
	Failed      int             `gorm:"column:failed;not null;default:0"`
	Skipped     int             `gorm:"column:skipped;not null;default:0"`
	SuccessRate decimal.Decimal `gorm:"column:success_rate;type:decimal(5,2);not null;default:0"`

	Cursor      string     `gorm:"column:cursor;type:varchar(64)"`
	TriggeredBy string     `gorm:"column:triggered_by;type:varchar(64)"`
	StartedAt   time.Time  `gorm:"column:started_at;type:datetime(3);not null;index:idx_sync_runs_started"`
	FinishedAt  *time.Time `gorm:"column:finished_at;type:datetime(3)"`
	DurationMS  int64      `gorm:"column:duration_ms;not null;default:0"`
	Error       string     `gorm:"column:error;type:text"`
	CreatedAt   time.Time  `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (SyncRunDto) TableName() string { return "sync_runs" }

func FromDomainSyncRun(r *entities.SyncRun) *SyncRunDto {
	dto := &SyncRunDto{
		ID:          r.ID,
		Kind:        r.Kind.String(),
		Market:      r.Market.String(),
		Status:      r.Status.String(),
		Total:       r.Stats.Total,
		Succeeded:   r.Stats.Succeeded,
		Failed:      r.Stats.Failed,
		Skipped:     r.Stats.Skipped,
		SuccessRate: r.Stats.SuccessRate,
		Cursor:      r.Cursor,
		TriggeredBy: r.TriggeredBy,
		StartedAt:   r.StartedAt,
		FinishedAt:  r.FinishedAt,
		DurationMS:  r.DurationMS,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
	// 空串要落成 NULL 而不是 ''：空串之间仍然互相冲突，
	// 那样第二次同步就再也插不进去了。
	if key := r.RunningKey(); key != "" {
		dto.RunningKey = &key
	}
	return dto
}

// ToDomain 重建聚合。
//
// 成功率直接读存量，不用 Succeeded/Total 重算：那是乘除派生值，
// 落库那一刻的值才是当时的事实。
func (dto SyncRunDto) ToDomain() *entities.SyncRun {
	return &entities.SyncRun{
		ID:     dto.ID,
		Kind:   value_objects.SyncKind(dto.Kind),
		Market: shared_vo.Market(dto.Market),
		Status: value_objects.SyncStatus(dto.Status),
		Stats: value_objects.RehydrateSyncStats(
			dto.Total, dto.Succeeded, dto.Failed, dto.Skipped, dto.SuccessRate,
		),
		Cursor:      dto.Cursor,
		TriggeredBy: dto.TriggeredBy,
		StartedAt:   dto.StartedAt,
		FinishedAt:  dto.FinishedAt,
		DurationMS:  dto.DurationMS,
		Error:       dto.Error,
		CreatedAt:   dto.CreatedAt,
		UpdatedAt:   dto.UpdatedAt,
	}
}

func ToDomainSyncRuns(rows []*SyncRunDto) []*entities.SyncRun {
	out := make([]*entities.SyncRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ToDomain())
	}
	return out
}
