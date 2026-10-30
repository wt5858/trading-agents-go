package entities

import (
	"fmt"
	"sync"

	"github.com/shopspring/decimal"
)

// CostBudget owns atomic estimated reservations for one run. It bounds concurrent
// admission, not a provider's final invoice. Unknown billed usage retains its reservation.
type CostBudget struct {
	mu                     sync.Mutex
	limit, spent, reserved decimal.Decimal
}

func NewCostBudget(limit decimal.Decimal) *CostBudget { return &CostBudget{limit: limit} }
func (b *CostBudget) Reserve(estimate decimal.Decimal) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if estimate.IsNegative() || b.spent.Add(b.reserved).Add(estimate).GreaterThan(b.limit) {
		return fmt.Errorf("run cost budget cannot admit next model request")
	}
	b.reserved = b.reserved.Add(estimate)
	return nil
}
func (b *CostBudget) Settle(reservation, actual decimal.Decimal, known bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved = b.reserved.Sub(reservation)
	if known {
		b.spent = b.spent.Add(actual)
	} else {
		b.spent = b.spent.Add(reservation)
	}
}
