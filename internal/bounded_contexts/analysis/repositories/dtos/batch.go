package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
)

// BatchDto 是 analysis_batches 表的持久化对象。
//
// 它与 analysis_tasks 之间没有外键：Batch 与 Task 是两个独立的聚合根，
// 外键会把它们绑进同一个事务，正是本次重构要拆掉的东西。
type BatchDto struct {
	ID     string `gorm:"column:id;type:varchar(40);primaryKey"`
	UserID uint64 `gorm:"column:user_id;not null;index:idx_batches_user,priority:1"`
	// 子任务 ID 列表用 JSON 数组存：批次创建后集合不再变化，
	// 而且 analysis_tasks.batch_id 上已有索引可以反查，没必要再维护一张关联表。
	TaskIDs []byte `gorm:"column:task_ids;type:json"`
	// 已结算的子任务 ID 集合，是结算幂等的依据。
	// 上限由 entities.MaxBatchSize 钉在百级，因此整份存 JSON 是安全的。
	SettledTaskIDs []byte `gorm:"column:settled_task_ids;type:json"`

	Total     int `gorm:"column:total;not null;default:0"`
	Completed int `gorm:"column:completed;not null;default:0"`
	Failed    int `gorm:"column:failed;not null;default:0"`
	// percent 是派生量，随聚合一起落库。读路径直接取，不再用 (completed+failed)/total 重算。
	Percent decimal.Decimal `gorm:"column:percent;type:decimal(5,2);not null;default:0"`
	// version 支撑乐观锁：并发结算时后写的 UPDATE 命中 0 行，由领域服务重试。
	Version int64 `gorm:"column:version;not null;default:0"`

	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;index:idx_batches_user,priority:2;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (BatchDto) TableName() string { return "analysis_batches" }

// FromDomainBatch 把批次聚合投影成 DTO。
func FromDomainBatch(b *entities.Batch) *BatchDto {
	return &BatchDto{
		ID:             b.ID,
		UserID:         b.UserID,
		TaskIDs:        marshalJSON(b.TaskIDs),
		SettledTaskIDs: marshalJSON(b.SettledTaskIDs),
		Total:          b.Total,
		Completed:      b.Completed,
		Failed:         b.Failed,
		Percent:        b.Percent,
		Version:        b.Version,
		CreatedAt:      b.CreatedAt,
		UpdatedAt:      b.UpdatedAt,
	}
}

// ToDomain 重建批次聚合。同样不做校验：库里的行是既成事实。
func (dto BatchDto) ToDomain() *entities.Batch {
	var taskIDs, settled []string
	unmarshalJSON(dto.TaskIDs, &taskIDs)
	unmarshalJSON(dto.SettledTaskIDs, &settled)
	return &entities.Batch{
		ID:             dto.ID,
		UserID:         dto.UserID,
		TaskIDs:        taskIDs,
		SettledTaskIDs: settled,
		Total:          dto.Total,
		Completed:      dto.Completed,
		Failed:         dto.Failed,
		Percent:        dto.Percent,
		Version:        dto.Version,
		CreatedAt:      dto.CreatedAt,
		UpdatedAt:      dto.UpdatedAt,
	}
}

func ToDomainBatches(rows []BatchDto) []*entities.Batch {
	out := make([]*entities.Batch, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
