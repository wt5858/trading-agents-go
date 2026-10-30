package entities

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

func TestConcurrentReservationsShareOneCeiling(t *testing.T) {
	budget := NewCostBudget(decimal.NewFromInt(1))
	outcomes, err := concurrency.Settle(context.Background(), []int{1, 2, 3, 4}, 4, func(context.Context, int) (bool, error) {
		err := budget.Reserve(decimal.RequireFromString("0.6"))
		return err == nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for _, o := range outcomes {
		if o.Value {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d overlapping reservations", admitted)
	}
	budget.Settle(decimal.RequireFromString("0.6"), decimal.RequireFromString("0.1"), true)
	if err := budget.Reserve(decimal.RequireFromString("0.9")); err != nil {
		t.Fatal(err)
	}
}
func TestUnknownBillingRetainsReservation(t *testing.T) {
	b := NewCostBudget(decimal.NewFromInt(1))
	if err := b.Reserve(decimal.NewFromInt(1)); err != nil {
		t.Fatal(err)
	}
	b.Settle(decimal.NewFromInt(1), decimal.Zero, false)
	if err := b.Reserve(decimal.RequireFromString("0.1")); err == nil {
		t.Fatal("unknown billed usage freed budget")
	}
}
