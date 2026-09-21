// Package domain_event_handlers 把分析上下文发出的领域事件接进报告上下文。
//
// 和 http_handlers 一样，这里只做「翻译 + 转调」：解事件、取字段、调用 domain_service。
// 任何判断（报告长什么样、章节怎么排）都在聚合里，不在这里。
//
// 本包唯一自己拿主意的事情是**投递语义**：一次重放到底算失败还是算成功。
// 那不是业务规则，而是「事件总线保证什么」的问题，所以它属于 application/ 而不是领域层。
package domain_event_handlers

import (
	"context"

	analysis_events "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/report/entities"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// ReportGenerator 是本处理器所需的窄能力，由消费方声明。
//
// 声明成接口而不是直接吃 *domain_services.ReportService：处理器的全部价值就在
// 「重放算不算成功」这一个判断上，而那正是最需要被测试覆盖的一行。
// 依赖具体服务意味着测一次重放要先有一个数据库。
type ReportGenerator interface {
	Generate(ctx context.Context, in domain_services.GenerateInput) (*entities.Report, error)
}

// Subscriber 是本处理器需要的订阅能力，由消费方声明，便于在测试里替换。
type Subscriber interface {
	RegisterSubscriber(h domain_event.Handler, prototype domain_event.DomainEvent)
}

// OnTaskCompletedHandler 是分析上下文与报告上下文之间的那根线。
//
// 两个上下文没有任何共享事务：分析任务自己落库并发出事件，报告在这里独立落库。
// 代价是最终一致（报告会比任务晚几毫秒出现），换来的是任何一侧的故障都不会
// 回滚另一侧——一次报告生成失败不该让一个跑了十几分钟的分析任务作废。
type OnTaskCompletedHandler struct {
	reports ReportGenerator
}

func NewOnTaskCompletedHandler(reports ReportGenerator) *OnTaskCompletedHandler {
	return &OnTaskCompletedHandler{reports: reports}
}

// Register 把处理器挂到事件总线上。
func (h *OnTaskCompletedHandler) Register(bus Subscriber) {
	bus.RegisterSubscriber(h.OnTaskCompleted, &analysis_events.OnTaskCompleted{})
}

// OnTaskCompleted 在分析任务成功收尾时生成报告。
//
// # 幂等
//
// 领域事件是**至少一次**投递的：总线重启后的补投、worker 可见性超时导致的重复执行、
// 以后换成 MQ 后的重投，都会让同一个 OnTaskCompleted 到达这里不止一次。
// 因此本处理器必须满足两件事——重放不能报错，重放也不能产出第二份报告。
//
// 实现方式是把这两件事都压到同一个东西上：analysis_reports.task_id 的唯一索引。
//
//   - 不能产出第二份：第二次 INSERT 撞唯一键，数据库直接拒绝。
//     这里刻意没有「先查有没有」——两次重放并发进来时，先查再插的两边都会查到
//     「还没有」，然后双双插入；唯一索引才是并发下真正成立的那个保证。
//   - 不能报错：仓储把冲突翻成 AlreadyExists，本方法把它当成功。
//     语义上这是对的——「这个任务的报告已经存在」正是本处理器想达成的状态，
//     谁把它写进去的并不重要。若照实返回错误，总线会把每一次正常重放
//     都记成一条处理失败的告警日志，真正的故障反而会被淹没在噪音里。
//
// 其余错误（分析结论查不到、数据库不可用）照实返回，交给总线记录与重试策略处理。
//
// 注意每次重放都会生成一个新的报告 ID。这不影响幂等：唯一性建立在 task_id 上，
// 而不是主键上，落败的那一次连同它的 ID 一起被丢弃，库里始终只有最先写入的那份。
func (h *OnTaskCompletedHandler) OnTaskCompleted(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*analysis_events.OnTaskCompleted)
	if !ok {
		// 订阅错了事件名是装配错误，不是运行期故障：静默忽略，别把总线搞崩。
		return nil
	}

	_, err := h.reports.Generate(ctx, domain_services.GenerateInput{
		TaskID: evt.TaskID,
		UserID: evt.UserID,
	})
	if err != nil && custom_errors.CodeOf(err) == custom_errors.CodeAlreadyExists {
		return nil
	}
	return err
}
