package domain_services

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

type costBudgetKey struct{}

func WithCostBudget(ctx context.Context, limit decimal.Decimal) context.Context {
	if !limit.IsPositive() {
		return ctx
	}
	return context.WithValue(ctx, costBudgetKey{}, entities.NewCostBudget(limit))
}
func reserveCost(ctx context.Context, client LLMClient, req vo.CompletionRequest) (func(*vo.CompletionResult), decimal.Decimal, error) {
	budget, _ := ctx.Value(costBudgetKey{}).(*entities.CostBudget)
	if budget == nil {
		return func(*vo.CompletionResult) {}, decimal.Zero, nil
	}
	estimator, ok := client.(interface {
		EstimateCost(vo.CompletionRequest) (decimal.Decimal, bool)
	})
	if !ok {
		return nil, decimal.Zero, fmt.Errorf("cost estimate unavailable under an active budget")
	}
	estimate, ok := estimator.EstimateCost(req)
	if !ok {
		return nil, decimal.Zero, fmt.Errorf("unknown model price under an active budget")
	}
	if err := budget.Reserve(estimate); err != nil {
		return nil, decimal.Zero, err
	}
	return func(result *vo.CompletionResult) {
		known := result != nil && (result.Usage.CostStatus == "estimated" || result.Usage.CostStatus == "mock")
		actual := decimal.Zero
		if result != nil {
			actual = result.Usage.CostUSD
		}
		budget.Settle(estimate, actual, known)
	}, estimate, nil
}
