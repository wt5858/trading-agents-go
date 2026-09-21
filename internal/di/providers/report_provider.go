package providers

import (
	"context"

	"github.com/google/wire"

	analysis_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	report_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/application/domain_event_handlers"
	report_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_services"
	report_repo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/repositories"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// AnalysisResultAdapter 把 analysis 上下文的任务仓储适配成报告上下文需要的窄端口。
//
// 适配器住在组装根，是为了让两个上下文都不必知道对方：report 只认
// AnalysisResultProvider 这个签名，analysis 完全不知道报告的存在。
type AnalysisResultAdapter struct {
	tasks *analysis_repo.TaskRepository
}

var _ report_services.AnalysisResultProvider = (*AnalysisResultAdapter)(nil)

func NewAnalysisResultAdapter(tasks *analysis_repo.TaskRepository) *AnalysisResultAdapter {
	return &AnalysisResultAdapter{tasks: tasks}
}

func (a *AnalysisResultAdapter) ResultOf(ctx context.Context, taskID string) (*analysis_vo.Result, error) {
	task, err := a.tasks.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Result == nil {
		// 任务存在但还没有结论：对报告上下文而言与「不存在」等价，
		// 用同一个错误码，免得调用方要分辨两种缺失。
		return nil, custom_errors.NotFound("任务 %s 尚无分析结论", taskID)
	}
	return task.Result, nil
}

var ReportSet = wire.NewSet(
	NewAnalysisResultAdapter,
	report_repo.NewReportRepository,
	report_services.NewReportService,

	wire.Bind(new(report_services.AnalysisResultProvider), new(*AnalysisResultAdapter)),
	// 绑定写在这里而不是 ReportDomainEventSet 里：Wire 要求 wire.Bind 与
	// 具体类型的 provider 同处一个 set，而 ReportService 是这个 set 提供的。
	// 用不到它的入口点带着一条多余的绑定，没有任何代价。
	wire.Bind(new(report_handlers.ReportGenerator), new(*report_services.ReportService)),
)

// ReportDomainEventSet 只在消费端需要：报告由「任务完成」事件驱动生成，
// 不开放直接创建接口。
var ReportDomainEventSet = wire.NewSet(
	report_handlers.NewOnTaskCompletedHandler,
)
