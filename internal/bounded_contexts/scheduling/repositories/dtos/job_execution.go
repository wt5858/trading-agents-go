package dtos

import (
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/value_objects"
)

// JobExecutionDto 是 job_executions 表的持久化对象。
//
// 它与 scheduled_jobs 之间**没有外键**：两者是独立的聚合根，
// 外键会把它们绑进同一个事务，也会让「删除一条任务」被迫连带处理几十万行历史。
// job_id 只是一个标识引用，上面有索引可以反查，这就够了。
//
// 这张表**不是**纯追加的：一行会被改写两次（queued -> running -> 终态）。
// 之所以能接受这两次 UPDATE，是因为它们都带着状态谓词（详见仓储层的
// ClaimQueued 与 Finish），因此每次改写都是一次原子的状态推进而不是覆盖，
// 「终态不可改写」这条不变式由 WHERE 子句本身保证，而不是靠调用方自觉。
//
// 清理路径仍然只有按时间的批量 DELETE，并且只碰终态之前很久的行，
// 因此归档作业和调度器实际上不会撞在同一批行上。
type JobExecutionDto struct {
	ID string `gorm:"column:id;type:varchar(40);primaryKey"`

	// idx_exec_job 支撑「某任务的执行历史，按时间倒序分页」这条唯一的列表查询。
	// (job_id, started_at) 让过滤和排序都走索引。
	JobID string `gorm:"column:job_id;type:varchar(40);not null;index:idx_exec_job,priority:1;uniqueIndex:uk_exec_occurrence,priority:1"`
	// idx_exec_running (status, started_at) 是 idx_exec_pending 的另一半。
	// ListStale 的条件是两支 OR：queued 看 queued_at，running 看 started_at。
	// 只给前一支建索引的话，MySQL 凑不出 index merge，整条查询退回全表扫描——
	// 而这是库里最大的表。两支各有所依，union 才成立。
	StartedAt time.Time `gorm:"column:started_at;type:datetime(3);not null;index:idx_exec_job,priority:2;index:idx_exec_started_at;index:idx_exec_running,priority:2"`

	// idx_exec_started_at 单列索引专供 PurgeOlderThan：
	// 保留期清理是 `DELETE WHERE started_at < ?`，不带 job_id，
	// 走不了上面那个复合索引的第一列。
	JobKind string `gorm:"column:job_kind;type:varchar(32);not null;default:''"`

	// uk_exec_occurrence (job_id, scheduled_for, attempt) 是一次尝试的天然主键。
	//
	// 它不是为了查询，是为了**挡住重复入队**：调度器在「CAS 抢到触发」与
	// 「写下 queued 记录」之间被杀掉再重启，恢复巡检会重新入队同一次触发；
	// 没有这个唯一索引，同一次触发就会有两条 queued 记录，于是被跑两遍。
	// 应用层的「先查一下在不在」挡不住它——那正是 TOCTOU。
	ScheduledFor time.Time `gorm:"column:scheduled_for;type:datetime(3);not null;uniqueIndex:uk_exec_occurrence,priority:2"`
	// idx_exec_pending (status, queued_at) 专供恢复巡检：
	// 「哪些记录卡在 queued/running 且已经卡了很久」。没有它，这个每分钟一次的
	// 巡检就是一次全表扫描，而这张表是六位数行起步的。
	QueuedAt   time.Time  `gorm:"column:queued_at;type:datetime(3);not null;index:idx_exec_pending,priority:2"`
	FinishedAt *time.Time `gorm:"column:finished_at;type:datetime(3)"`

	Attempt int `gorm:"column:attempt;not null;default:1;uniqueIndex:uk_exec_occurrence,priority:3"`

	Status    string `gorm:"column:status;type:varchar(16);not null;index:idx_exec_pending,priority:1;index:idx_exec_running,priority:1"`
	Summary   string `gorm:"column:summary;type:varchar(512);not null;default:''"`
	ItemCount int    `gorm:"column:item_count;not null;default:0"`
	Error     string `gorm:"column:error;type:varchar(512);not null;default:''"`

	// duration_ms 是派生量，随记录一起落库。读路径直接取，
	// 不用 finished_at - started_at 重算：SQL 层的时间差计算在跨时区/跨精度时
	// 会给出和应用层不一致的结果，而这一列是耗时看板的唯一数据源。
	DurationMs int64 `gorm:"column:duration_ms;not null;default:0"`

	Manual bool `gorm:"column:manual;not null;default:false"`
}

func (JobExecutionDto) TableName() string { return "job_executions" }

// FromDomainExecution 把执行记录聚合投影成 DTO。
func FromDomainExecution(e *entities.JobExecution) *JobExecutionDto {
	return &JobExecutionDto{
		ID:           e.ID,
		JobID:        e.JobID,
		JobKind:      e.JobKind.String(),
		ScheduledFor: e.ScheduledFor,
		QueuedAt:     e.QueuedAt,
		StartedAt:    e.StartedAt,
		FinishedAt:   e.FinishedAt,
		Attempt:      e.Attempt,
		Status:       e.Status.String(),
		Summary:      e.Summary,
		ItemCount:    e.ItemCount,
		Error:        e.ErrMsg,
		DurationMs:   e.DurationMs,
		Manual:       e.Manual,
	}
}

// ToDomain 重建执行记录聚合。同样不做校验：库里的行是既成事实。
func (dto JobExecutionDto) ToDomain() *entities.JobExecution {
	return &entities.JobExecution{
		ID:           dto.ID,
		JobID:        dto.JobID,
		JobKind:      value_objects.JobKind(dto.JobKind),
		ScheduledFor: dto.ScheduledFor,
		QueuedAt:     dto.QueuedAt,
		StartedAt:    dto.StartedAt,
		FinishedAt:   dto.FinishedAt,
		Attempt:      dto.Attempt,
		Status:       value_objects.ExecutionStatus(dto.Status),
		Summary:      dto.Summary,
		ItemCount:    dto.ItemCount,
		ErrMsg:       dto.Error,
		DurationMs:   dto.DurationMs,
		Manual:       dto.Manual,
	}
}

func ToDomainExecutions(rows []JobExecutionDto) []*entities.JobExecution {
	out := make([]*entities.JobExecution, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToDomain())
	}
	return out
}
