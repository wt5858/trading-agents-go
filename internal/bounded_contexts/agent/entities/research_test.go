package entities

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/concurrency"
)

func TestResearchBudgetAtomicReservationsAndUnknownUsage(t *testing.T) {
	l := vo.DefaultResearchLimits()
	l.MaxRequests = 3
	l.MaxTokens = 1000
	b := NewResearchBudget(l)
	var admitted atomic.Int32
	_, err := concurrency.Settle(context.Background(), make([]int, 20), 4, func(context.Context, int) (struct{}, error) {
		settle, err := b.Reserve(400)
		if err == nil {
			admitted.Add(1)
			settle(&vo.Usage{})
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Load() != 2 {
		t.Fatalf("unknown usage released reserved tokens: %d", admitted.Load())
	}
	if _, err := b.Reserve(201); !errors.Is(err, ErrResearchBudget) {
		t.Fatal("token cap bypassed")
	}
}
func TestResearchBudgetRollbackBeforeNetworkDoesNotSpendRequest(t *testing.T) {
	l := vo.DefaultResearchLimits()
	l.MaxRequests = 3
	l.MaxTokens = 1000
	b := NewResearchBudget(l)
	for i := 0; i < 5; i++ {
		settle, err := b.Reserve(900)
		if err != nil {
			t.Fatal(err)
		}
		settle(nil)
	}
	for i := 0; i < 3; i++ {
		settle, err := b.Reserve(900)
		if err != nil {
			t.Fatal(err)
		}
		settle(&vo.Usage{TotalTokens: 10})
	}
	if _, err := b.Reserve(1); !errors.Is(err, ErrResearchBudget) {
		t.Fatal("request cap bypassed")
	}
}
