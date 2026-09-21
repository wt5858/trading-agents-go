// Package domain_event_handlers 把分析上下文自己发出的领域事件接回领域服务。
//
// 和 http_handlers 一样，这里只做「翻译 + 转调」：解事件、取字段、调用 domain_service。
// 任何判断（能不能结算、算不算重复）都在聚合里，不在这里。
package domain_event_handlers

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/entities"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
)

// BatchSettlementHandler 是 Task 与 Batch 两个聚合根之间的那根线。
//
// 它取代了旧实现里的跨聚合事务：子任务收尾时不再由 worker 顺手去改批次计数，
// 而是各自落库、由这里凭事件把结局登记进批次。两个聚合因此没有任何共享事务，
// 也不需要「恰好一次」的投递保证——Batch.Settle 是幂等的，重放无害。
type BatchSettlementHandler struct {
	batchService *domain_services.BatchService
}

func NewBatchSettlementHandler(batchService *domain_services.BatchService) *BatchSettlementHandler {
	return &BatchSettlementHandler{batchService: batchService}
}

// Subscriber 是本处理器需要的订阅能力，由消费方声明，便于在测试里替换。
type Subscriber interface {
	RegisterSubscriber(h domain_event.Handler, prototype domain_event.DomainEvent)
}

// Register 把处理器挂到事件总线上。
func (h *BatchSettlementHandler) Register(bus Subscriber) {
	bus.RegisterSubscriber(h.OnTaskCompleted, &domain_events.OnTaskCompleted{})
	bus.RegisterSubscriber(h.OnTaskFailed, &domain_events.OnTaskFailed{})
	bus.RegisterSubscriber(h.OnTaskCanceled, &domain_events.OnTaskCanceled{})
}

func (h *BatchSettlementHandler) OnTaskCompleted(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*domain_events.OnTaskCompleted)
	if !ok || evt.BatchID == "" {
		return nil
	}
	return h.batchService.SettleTask(ctx, evt.BatchID, evt.TaskID, entities.SettleCompleted)
}

// OnTaskFailed 只在「不会再重试」时登记。
//
// 一次还会重试的失败不是子任务的结局：提前记进失败计数，等重试成功后
// completed+failed 就会超过 total，批次的结算数直接对不上。
// 这个判断由聚合在发事件时给出（Task.Fail 会把 Retryable 写进事件），
// 处理器只负责照着做。
func (h *BatchSettlementHandler) OnTaskFailed(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*domain_events.OnTaskFailed)
	if !ok || evt.BatchID == "" || evt.Retryable {
		return nil
	}
	return h.batchService.SettleTask(ctx, evt.BatchID, evt.TaskID, entities.SettleFailed)
}

// OnTaskCanceled 把取消也算作一次失败结算：对批次而言，
// 「用户取消」和「跑失败了」同样意味着这个子任务不会再产出结果，
// 批次不该为它继续等下去。
func (h *BatchSettlementHandler) OnTaskCanceled(ctx context.Context, e domain_event.DomainEvent) error {
	evt, ok := e.(*domain_events.OnTaskCanceled)
	if !ok || evt.BatchID == "" {
		return nil
	}
	return h.batchService.SettleTask(ctx, evt.BatchID, evt.TaskID, entities.SettleFailed)
}
