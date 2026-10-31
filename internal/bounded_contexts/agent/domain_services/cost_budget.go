package domain_services

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
)

type costBudgetKey struct{}
type armBudgetKey struct{}
type pairBudgetKey struct{}

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
	arm, _ := ctx.Value(armBudgetKey{}).(*entities.CostBudget)
	if arm != nil {
		if err := arm.Reserve(estimate); err != nil {
			budget.Settle(estimate, decimal.Zero, true)
			return nil, decimal.Zero, err
		}
	}
	return func(result *vo.CompletionResult) {
		known := result != nil && (result.Usage.CostStatus == "estimated" || result.Usage.CostStatus == "mock")
		actual := decimal.Zero
		if result != nil {
			actual = result.Usage.CostUSD
		}
		budget.Settle(estimate, actual, known)
		if arm != nil {
			arm.Settle(estimate, actual, known)
		}
	}, estimate, nil
}

func armAllowance(ctx context.Context) decimal.Decimal {
	if v, ok := ctx.Value(pairBudgetKey{}).(decimal.Decimal); ok {
		return v
	}
	if parent, ok := ctx.Value(costBudgetKey{}).(*entities.CostBudget); ok {
		return parent.Limit().DivRound(decimal.NewFromInt(2), 8)
	}
	return decimal.Zero
}
func armContext(ctx context.Context, allowance decimal.Decimal) context.Context {
	if allowance.IsPositive() {
		return context.WithValue(ctx, armBudgetKey{}, entities.NewCostBudget(allowance))
	}
	return ctx
}
