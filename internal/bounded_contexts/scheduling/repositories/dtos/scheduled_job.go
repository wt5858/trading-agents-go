// Package dtos 是定时任务上下文的持久化形状：表结构、JSON 列格式，以及与聚合的双向映射。
//
// 映射写在 DTO 文件里（DTO 上挂 ToDomain()，包级函数 FromDomainXxx()），
// 不另开 mapper 包：映射与它服务的表结构必须同生共死，拆开只会让改一次列
// 要动两个目录，还容易漏。
//
// 铁律：DTO 绝不越过 repositories/ 这一层。上层拿到的永远是聚合或值对象。
package dtos

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
)

// 编译期断言：DTO 必须自带表名，漏写会让 GORM 按结构体名推导出错误的表。
var (
	_ interface{ TableName() string } = ScheduledJobDto{}
	_ interface{ TableName() string } = JobExecutionDto{}
)

// ScheduledJobDto 是 scheduled_jobs 表的持久化对象。
//
// 通用约定与既有上下文保持一致：时间统一 datetime(3)；时间戳由聚合自己维护，
// 因此关掉 GORM 的 autoCreateTime/autoUpdateTime——否则导入存量任务时
// created_at 会被 GORM 静默改写成导入时刻。
type ScheduledJobDto struct {
	ID string `gorm:"column:id;type:varchar(40);primaryKey"`

	// name 唯一。这不只是防手滑：运维排查时是按名字找任务的，
	// 出现两条同名任务时「暂停那个行情同步」会变成一次赌博。
	Name string `gorm:"column:name;type:varchar(128);not null;uniqueIndex:uk_jobs_name"`

	// idx_jobs_due 是调度器的命脉：ClaimDue 每个 tick 都会跑
	// `WHERE status = 'enabled' AND next_run_at <= ? ORDER BY next_run_at`。
	// (status, next_run_at) 这个顺序让它既能走索引过滤又能走索引排序，
	// 完全避免了在一张会长到几万行的表上做全表扫 + filesort。
	Status    string    `gorm:"column:status;type:varchar(16);not null;index:idx_jobs_due,priority:1"`
	NextRunAt time.Time `gorm:"column:next_run_at;type:datetime(3);not null;index:idx_jobs_due,priority:2"`

	// (kind, status) 服务管理界面的筛选，与 idx_jobs_due 的职责不同，不要合并：
	// 合并成一个三列索引之后，调度器的热查询就得带上 kind 才能走全索引。
	Kind string `gorm:"column:kind;type:varchar(32);not null;index:idx_jobs_kind_status,priority:1"`

	// cron_spec 存原文而不是解析结果：解析结果是派生物，
	// 而原文是管理员输入的、需要原样回显的权威数据。
	CronSpec string `gorm:"column:cron_spec;type:varchar(128);not null"`
	// payload 是运行器自己解释的不透明参数，整体读写，用 JSON 列。
	Payload []byte `gorm:"column:payload;type:json"`

	LastRunAt  *time.Time `gorm:"column:last_run_at;type:datetime(3)"`
	LastStatus string     `gorm:"column:last_status;type:varchar(16);not null;default:''"`

	ConsecutiveFailures    int `gorm:"column:consecutive_failures;not null;default:0"`
	MaxConsecutiveFailures int `gorm:"column:max_consecutive_failures;not null;default:5"`

	// 超时存秒而不是 time.Duration（纳秒）：纳秒数在 SQL 客户端里没法读，
	// 而这一列是运维会直接手改的少数几列之一。
	TimeoutSeconds int `gorm:"column:timeout_seconds;not null;default:600"`

	TotalRuns   int64 `gorm:"column:total_runs;not null;default:0"`
	SuccessRuns int64 `gorm:"column:success_runs;not null;default:0"`
	// success_rate 是派生量，随聚合一起落库。读路径直接取，
	// 不再用 success_runs/total_runs 重算——理由见 entities.ScheduledJob 的注释。
	SuccessRate decimal.Decimal `gorm:"column:success_rate;type:decimal(5,2);not null;default:0"`

	// created_by 只存 identity 上下文的用户 ID，没有也不该有外键：
	// 跨上下文只按标识引用，外键会把两个上下文绑进同一个数据库生命周期。
	CreatedBy uint64    `gorm:"column:created_by;not null;index:idx_jobs_creator"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:datetime(3);not null;autoUpdateTime:false"`
}

func (ScheduledJobDto) TableName() string { return "scheduled_jobs" }

// FromDomainJob 把任务聚合投影成 DTO。
//
// 注意这里没有 ClaimedFor 的对应列：那是一次抢占在内存里的凭据，
// 它的持久化形态是 job_executions.scheduled_for。见聚合上的注释。
func FromDomainJob(j *entities.ScheduledJob) *ScheduledJobDto {
	payload, err := j.Payload.ToJSON()
	if err != nil {
		// 参数在构造 VO 时已经序列化成功过一次，走到这里只可能是不可恢复的编程错误。
		// 退化成 NULL 列，好过让一次保存因为序列化而失败。
		payload = nil
	}
	return &ScheduledJobDto{
		ID:                     j.ID,
		Name:                   j.Name,
		Kind:                   j.Kind.String(),
		CronSpec:               j.Cron.String(),
		Payload:                payload,
		Status:                 j.Status.String(),
		NextRunAt:              j.NextRunAt,
		LastRunAt:              j.LastRunAt,
		LastStatus:             j.LastStatus.String(),
		ConsecutiveFailures:    j.ConsecutiveFailures,
		MaxConsecutiveFailures: j.MaxConsecutiveFailures,
		TimeoutSeconds:         int(j.EffectiveTimeout() / time.Second),
		TotalRuns:              j.TotalRuns,
		SuccessRuns:            j.SuccessRuns,
		SuccessRate:            j.SuccessRate,
		CreatedBy:              j.CreatedBy,
		CreatedAt:              j.CreatedAt,
		UpdatedAt:              j.UpdatedAt,
	}
}

// ToDomain 把 DTO 重建成聚合根。
//
// 这里不做校验：数据库里的行是既成事实。用 VO 的校验构造器去解析它，
// 会让一条 cron 表达式写坏了的历史记录把整个任务列表接口打挂——
// 而那条记录恰恰是运维最需要看到、最需要能改掉的那一条。
// 校验属于写入路径，所以这里走 Rehydrate 系列。
func (dto ScheduledJobDto) ToDomain() *entities.ScheduledJob {
	return &entities.ScheduledJob{
		ID:                     dto.ID,
		Name:                   dto.Name,
		Kind:                   value_objects.JobKind(dto.Kind),
		Cron:                   value_objects.RehydrateCronExpression(dto.CronSpec),
		Payload:                value_objects.RehydrateJobPayload(dto.Payload),
		Status:                 value_objects.JobStatus(dto.Status),
		NextRunAt:              dto.NextRunAt,
		LastRunAt:              dto.LastRunAt,
		LastStatus:             value_objects.ExecutionStatus(dto.LastStatus),
		ConsecutiveFailures:    dto.ConsecutiveFailures,
		MaxConsecutiveFailures: dto.MaxConsecutiveFailures,
		Timeout:                time.Duration(dto.TimeoutSeconds) * time.Second,
		TotalRuns:              dto.TotalRuns,
		SuccessRuns:            dto.SuccessRuns,
		// 成功率整份读回，不用 success_runs/total_runs 重算：它是写入当时固化的事实。
		SuccessRate: dto.SuccessRate,
		CreatedBy:   dto.CreatedBy,
		CreatedAt:   dto.CreatedAt,
		UpdatedAt:   dto.UpdatedAt,
	}
}

func ToDomainJobs(rows []ScheduledJobDto) []*entities.ScheduledJob {
	out := make([]*entities.ScheduledJob, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
