package providers

import (
	"github.com/google/wire"

	analysis_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	scheduling_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/scheduling/domain_services"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
)

// WorkerRunners 是 worker 进程要拉起来的两个常驻循环，外加一个启动期清理动作。
//
// 它存在的理由与 event_handlers 里那两个聚合器相同：Wire 只造被需要的东西，
// 而这三样彼此之间没有依赖关系，必须由一个结构把它们同时「需要」出来。
//
// 为什么不放进 internal/helpers/event_handlers：那个包的名字说的是事件处理器，
// 而常驻循环不是。包名撒谎的代价，是下一个人要读完整个包才知道里面有什么。
type WorkerRunners struct {
	// Analysis 从 Redis 队列取分析任务并跑完整条流水线。
	Analysis *analysis_services.WorkerService
	// Scheduler 到点把定时任务投进消息队列，自己不执行任何业务。
	Scheduler *scheduling_services.SchedulerService
	// Sync 只用于启动期清理僵死同步记录，不是常驻循环。
	Sync *stock_services.SyncService
}

func NewWorkerRunners(
	analysis *analysis_services.WorkerService,
	scheduler *scheduling_services.SchedulerService,
	sync *stock_services.SyncService,
) *WorkerRunners {
	return &WorkerRunners{Analysis: analysis, Scheduler: scheduler, Sync: sync}
}

var WorkerSet = wire.NewSet(NewWorkerRunners)
