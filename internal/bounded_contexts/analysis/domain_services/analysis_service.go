package domain_services

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

// Policy 是分析上下文的运行策略。
type Policy struct {
	Limits Limits
	// MaxAttempts 是失败任务的最大尝试次数，超过后不再重新排队。
	MaxAttempts int
}

func (p Policy) normalized() Policy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	return p
}

// AnalysisService 编排单个任务的提交、查询与取消。
//
// 批量提交不在这里：它要协调 Task 与 Batch 两个聚合根，逻辑与一致性故事
// 都自成一体，放在 BatchService。
type AnalysisService struct {
	tasks      *repositories.TaskRepository
	dispatcher *repositories.TaskDispatcher
	progress   *repositories.ProgressPublisher
	guard      *repositories.ConcurrencyGuard
	publisher  domain_event.Publisher
	policy     Policy
}

func NewAnalysisService(
	tasks *repositories.TaskRepository,
	dispatcher *repositories.TaskDispatcher,
	progress *repositories.ProgressPublisher,
	guard *repositories.ConcurrencyGuard,
	publisher domain_event.Publisher,
	policy Policy,
) *AnalysisService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &AnalysisService{
		tasks: tasks, dispatcher: dispatcher, progress: progress,
		guard: guard, publisher: publisher, policy: policy.normalized(),
	}
}

// SubmitInput 是提交单次分析的入参形状。
type SubmitInput struct {
	Code      string
	Market    string
	TradeDate string
	Depth     int
	Analysts  []string
	LLMModel  string
}

// Submit 提交一次分析。
//
// 顺序是刻意的：形状校验 -> 创建聚合 -> 占并发名额 -> 落库 -> 派发 -> 发事件。
//
//   - 先创建聚合再占名额：名额是按任务 ID 发放的凭据，没有 ID 就没法发凭据，
//     也就没法保证后面的归还恰好一次。
//   - 先落库再派发：消费端收到消息后的第一件事就是按 ID 认领任务；
//     反过来的话，消息里会出现一个数据库里还不存在的 ID，而它认领不到任何东西。
func (s *AnalysisService) Submit(ctx context.Context, op Operator, in SubmitInput) (*entities.Task, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	req, err := s.buildRequest(in)
	if err != nil {
		return nil, err
	}
	task, err := entities.NewTask(idx.TaskID(), op.UserID, req)
	if err != nil {
		return nil, err
	}

	if err := s.guard.Acquire(ctx, op.UserID, []string{task.ID}, s.policy.Limits.PerUser, s.policy.Limits.Global); err != nil {
		return nil, err
	}

	if err := s.tasks.Save(ctx, task); err != nil {
		s.release(ctx, op.UserID, task.ID)
		return nil, err
	}
	if err := s.dispatcher.Dispatch(ctx, task.ID); err != nil {
		// 已落库但消息没发出去：标记失败而不是留一条永远排队的僵尸记录。
		// maxAttempts 传 0——没有消息就没人会来跑它，事件必须如实说明。
		//
		// 注意这里和停滞巡检是两条互补的路径，不是重复：这里能当场知道投递失败，
		// 所以立刻给用户一个明确的失败；巡检兜的是「进程在这两步之间被杀掉」，
		// 那种情况下没有任何人能执行下面这段代码。
		if markErr := task.Fail("任务派发失败", 0); markErr == nil {
			_ = s.tasks.Update(ctx, task)
		}
		s.release(ctx, op.UserID, task.ID)
		s.publish(ctx, task)
		return nil, err
	}

	// 事件在领域决策与落库都完成之后才发布。
	s.publish(ctx, task)
	return task, nil
}

// GetTask 按 ID 取任务，附带归属校验。
func (s *AnalysisService) GetTask(ctx context.Context, op Operator, taskID string) (*entities.Task, error) {
	task, err := s.tasks.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnership(op, task); err != nil {
		return nil, err
	}
	return task, nil
}

// GetResult 取分析结果。未完成的任务没有结果，这不是错误而是状态。
func (s *AnalysisService) GetResult(ctx context.Context, op Operator, taskID string) (*value_objects.Result, error) {
	task, err := s.GetTask(ctx, op, taskID)
	if err != nil {
		return nil, err
	}
	if task.Result == nil {
		return nil, custom_errors.NotFound("任务 %s 尚未产出分析结果（当前状态：%s）",
			taskID, task.Status.DisplayName())
	}
	return task.Result, nil
}

// Cancel 取消任务。
//
// 只改状态，不去队列里捞那条消息：Redis 的 list 不支持高效的按值删除，
// worker 出队后会先读任务，发现已取消就直接 Ack 丢弃——这比维护一个
// 「已取消集合」简单得多，代价只是一次空转的出队。
func (s *AnalysisService) Cancel(ctx context.Context, op Operator, taskID string) error {
	task, err := s.tasks.FindByID(ctx, taskID)
	if err != nil {
		return err
	}
	if err := s.requireOwnership(op, task); err != nil {
		return err
	}
	// 领域决策：能不能取消由聚合判定（终态任务不可取消）。
	if err := task.Cancel(); err != nil {
		return err
	}
	// 落库这一步同时是并发仲裁：仓储把「终局不可改写」写进了 UPDATE 的 WHERE，
	// 若 worker 已经抢先写入完成状态，这里会拿到 Conflict 而不是覆盖掉结果。
	if err := s.tasks.Update(ctx, task); err != nil {
		return err
	}
	// 名额随取消归还。放在落库之后：落库失败时任务仍在运行，名额不该被释放。
	// 这里和 worker 的收尾路径可能同时触发，但凭据机制保证只有一方真正扣减。
	s.release(ctx, task.UserID, task.ID)

	s.publish(ctx, task)
	return nil
}

// ListByUser 列出用户的任务。管理员可以查别人的，普通用户只能查自己的。
func (s *AnalysisService) ListByUser(ctx context.Context, op Operator, targetUserID uint64, status string, page shared_vo.Page) ([]*entities.Task, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	if targetUserID == 0 {
		targetUserID = op.UserID
	}
	if targetUserID != op.UserID && !op.IsAdmin {
		return nil, 0, custom_errors.Forbidden("无权查看其他用户的分析任务")
	}
	st, err := value_objects.NewStatus(status)
	if err != nil {
		return nil, 0, err
	}
	return s.tasks.ListByUser(ctx, targetUserID, st, page)
}

// ProgressSubscription 是一次进度订阅：当前快照 + 后续增量流 + 任务是否已终结。
//
// Terminal 让调用方知道「不必再等了」：一个已取消或已失败的任务不会再推进度，
// 没有这个标志，SSE 连接会一直挂到客户端自己超时。
type ProgressSubscription struct {
	Snapshot value_objects.Progress
	Terminal bool
	Stream   *repositories.ProgressStream
}

// SubscribeProgress 订阅任务进度。
//
// # 为什么返回快照 + 流，而不是合成一个 channel
//
// 合成一个通道就需要一个常驻协程把首帧和后续增量缝在一起——那是一个裸 goroutine，
// 生命周期还和调用方的 HTTP 连接绑死，调用方一旦忘记取消就永久泄漏。
// 拆成两件东西之后，调用方先写首帧、再循环拉流，整条链路一个额外协程都不需要。
//
// 快照的来源优先级：Redis 快照（最新）> 任务聚合里的进度（落库那一刻的）。
// 两者都是写入时固化好百分比的完整快照，直接用，不重算。
//
// 返回的 Stream 必须 Close，否则订阅连接会泄漏。
func (s *AnalysisService) SubscribeProgress(ctx context.Context, op Operator, taskID string) (*ProgressSubscription, error) {
	task, err := s.GetTask(ctx, op, taskID)
	if err != nil {
		return nil, err
	}

	// 先订阅再读快照：反过来的话，两步之间推进的那一帧会两头都收不到。
	stream, err := s.progress.Subscribe(ctx, taskID)
	if err != nil {
		return nil, err
	}

	snapshot := task.Progress
	if snap, err := s.progress.Snapshot(ctx, taskID); err == nil && snap != nil {
		snapshot = *snap
	}
	return &ProgressSubscription{
		Snapshot: snapshot,
		Terminal: task.Status.Terminal(),
		Stream:   stream,
	}, nil
}

// buildRequest 把接口层的原始形状转成校验过的值对象。
func (s *AnalysisService) buildRequest(in SubmitInput) (value_objects.Request, error) {
	code, err := shared_vo.NewStockCode(in.Code, shared_vo.Market(in.Market))
	if err != nil {
		return value_objects.Request{}, err
	}
	tradeDate, err := shared_vo.NewTradeDate(in.TradeDate)
	if err != nil {
		return value_objects.Request{}, err
	}
	return value_objects.NewRequest(code, tradeDate, value_objects.Depth(in.Depth), in.Analysts, in.LLMModel)
}

// requireOwnership 是本层的权限判定。
func (s *AnalysisService) requireOwnership(op Operator, task *entities.Task) error {
	if err := requireLogin(op); err != nil {
		return err
	}
	if op.IsAdmin || task.OwnedBy(op.UserID) {
		return nil
	}
	// 返回 NotFound 而不是 Forbidden：Forbidden 等于确认「这个 ID 存在」，
	// 会让任务 ID 空间变成可探测的信道。
	return custom_errors.NotFound("分析任务(id=%s) 不存在", task.ID)
}

// release 归还名额。忽略错误是有意的：名额有 TTL 兜底，
// 为一次配额归还失败而让用户看到「取消失败」是本末倒置。
func (s *AnalysisService) release(ctx context.Context, userID uint64, taskIDs ...string) {
	_, _ = s.guard.Release(ctx, userID, taskIDs...)
}

// publish 取出聚合累积的事件并发布。取出即清空，保证不会重复发布。
func (s *AnalysisService) publish(ctx context.Context, t *entities.Task) {
	if evts := t.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}
