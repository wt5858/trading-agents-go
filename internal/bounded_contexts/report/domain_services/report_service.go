package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// ReportService 编排报告的生成、查询与删除。
type ReportService struct {
	reports   *repositories.ReportRepository
	analysis  AnalysisResultProvider
	publisher domain_event.Publisher
}

func NewReportService(
	reports *repositories.ReportRepository,
	analysis AnalysisResultProvider,
	publisher domain_event.Publisher,
) *ReportService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &ReportService{reports: reports, analysis: analysis, publisher: publisher}
}

// GenerateInput 是生成报告的入参形状。
//
// 只收两个标识，不收 Result：结论必须由本服务向分析上下文回查，而不是由调用方
// （事件处理器）转手带进来。事件是个快照，里面的数字可能是几分钟前的；
// 报告必须基于落库的权威结论生成。
type GenerateInput struct {
	TaskID string
	UserID uint64
}

// Generate 从一次已完成的分析生成报告。
//
// 顺序是刻意的：回查结论 -> 聚合投影 -> 落库 -> 发事件。
//
// # 关于重复生成
//
// 这里没有「这个任务是不是已经有报告了」的前置查询。那种写法是 TOCTOU：
// 事件至少投递一次，两次重放并发进来会双双查到「还没有」，然后双双插入。
// 真正的保证是 analysis_reports.task_id 上的唯一索引，仓储会把冲突翻成 AlreadyExists。
//
// 本方法如实把 AlreadyExists 抛出去，不在这里吞掉：对一个「重建报告」的运维接口
// 而言，重复是必须被告知的事实。至于「事件重放导致的重复是良性的」，那是投递语义的
// 问题，归事件处理器判断——见 application/domain_event_handlers。
func (s *ReportService) Generate(ctx context.Context, in GenerateInput) (*entities.Report, error) {
	if in.TaskID == "" {
		return nil, custom_errors.Invalid("任务 ID 不能为空")
	}

	result, err := s.analysis.ResultOf(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, custom_errors.NotFound("分析任务(id=%s) 尚未产出结论，无法生成报告", in.TaskID)
	}

	// 章节构成、排序与结论措辞都是业务规则，全部在聚合的构造器里，本层不参与。
	report, err := entities.FromAnalysisResult(idx.ReportID(), in.UserID, in.TaskID, *result)
	if err != nil {
		return nil, err
	}

	if err := s.reports.Create(ctx, report); err != nil {
		return nil, err
	}

	// 事件在领域决策与落库都完成之后才发布。撞唯一键的那一次在上面就返回了，
	// 因此 OnReportGenerated 只会由真正写成功的那一方发出，不会重复广播。
	s.publish(ctx, report)
	return report, nil
}

// Get 取一份报告，附带归属校验。
func (s *ReportService) Get(ctx context.Context, op Operator, reportID string) (*entities.Report, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	report, err := s.reports.FindByID(ctx, reportID)
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnership(op, report); err != nil {
		return nil, err
	}
	return report, nil
}

// GetSection 取报告的某一章节。
//
// 先按完整报告做归属校验再取章节：章节没有独立身份，它的可见性完全取决于
// 所属报告的可见性。这也意味着这里天然不存在「章节级越权」这种漏洞面。
func (s *ReportService) GetSection(ctx context.Context, op Operator, reportID string, key value_objects.SectionKey) (value_objects.Section, error) {
	report, err := s.Get(ctx, op, reportID)
	if err != nil {
		return value_objects.Section{}, err
	}
	// 章节存不存在由聚合判定。
	return report.SectionOf(key)
}

// ListByUser 列出报告。管理员可以查别人的，普通用户只能查自己的。
func (s *ReportService) ListByUser(ctx context.Context, op Operator, targetUserID uint64, page shared_vo.Page) ([]*entities.Report, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	if targetUserID == 0 {
		targetUserID = op.UserID
	}
	if targetUserID != op.UserID && !op.IsAdmin {
		return nil, 0, custom_errors.Forbidden("无权查看其他用户的分析报告")
	}
	return s.reports.ListByUser(ctx, targetUserID, page)
}

// Delete 删除一份报告。
//
// 这里不先把报告读出来判归属：归属被交给仓储写进 DELETE 的 WHERE 谓词，
// 检查与删除因此是同一个原子操作。先读后删在并发下会打在一条本不该删的行上。
//
// 管理员不受归属限制，传 0 表示放开谓词。
func (s *ReportService) Delete(ctx context.Context, op Operator, reportID string) error {
	if err := requireLogin(op); err != nil {
		return err
	}
	owner := op.UserID
	if op.IsAdmin {
		owner = 0
	}
	// 零行命中时仓储返回 NotFound（而不是 Forbidden），理由同 requireOwnership。
	return s.reports.Delete(ctx, reportID, owner)
}

// requireOwnership 是本层的权限判定。
func (s *ReportService) requireOwnership(op Operator, report *entities.Report) error {
	if op.IsAdmin || report.OwnedBy(op.UserID) {
		return nil
	}
	// 返回 NotFound 而不是 Forbidden：Forbidden 等于确认「这个 ID 存在」，
	// 会让报告 ID 空间变成可探测的信道——攻击者可以靠状态码差异枚举出
	// 系统里有哪些报告，以及别人在分析什么股票。
	return custom_errors.NotFound("分析报告(id=%s) 不存在", report.ID)
}

// publish 取出聚合累积的事件并发布。取出即清空，保证不会重复发布。
func (s *ReportService) publish(ctx context.Context, r *entities.Report) {
	if evts := r.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}
