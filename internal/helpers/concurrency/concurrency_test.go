package concurrency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMapPreservesInputOrder(t *testing.T) {
	// Deliberately make later items finish first; results must still line up with input.
	items := []int{1, 2, 3, 4, 5}
	got, err := Map(context.Background(), items, 5, func(_ context.Context, n int) (int, error) {
		time.Sleep(time.Duration(6-n) * 5 * time.Millisecond)
		return n * 10, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{10, 20, 30, 40, 50}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestMapRespectsConcurrencyLimit(t *testing.T) {
	const limit = 3
	var (
		mu      sync.Mutex
		current int
		peak    int
	)
	items := make([]int, 50)

	_, err := Map(context.Background(), items, limit, func(_ context.Context, _ int) (int, error) {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()

		time.Sleep(2 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()
		return 0, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peak > limit {
		t.Fatalf("peak concurrency %d exceeded limit %d", peak, limit)
	}
}

func TestMapFailsFastAndCancelsInFlightPeers(t *testing.T) {
	boom := errors.New("boom")
	var observedCancel atomic.Bool

	// Item 0 fails only after the peers are already inside fn, so this exercises
	// cancellation of in-flight work rather than the cheaper "never dispatched" path.
	started := make(chan struct{}, 3)
	items := []int{0, 1, 2, 3}
	_, err := Map(context.Background(), items, 4, func(ctx context.Context, n int) (int, error) {
		if n == 0 {
			for i := 0; i < 3; i++ {
				<-started
			}
			return 0, boom
		}
		started <- struct{}{}
		select {
		case <-ctx.Done():
			observedCancel.Store(true)
			return 0, ctx.Err()
		case <-time.After(2 * time.Second):
			return n, nil
		}
	})

	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the triggering error to be wrapped, got %v", err)
	}
	if !observedCancel.Load() {
		t.Fatal("expected in-flight peers to observe cancellation after the first failure")
	}
}

// Map is fail-fast, so it cannot promise an error per item — the first failure cancels
// the rest. Settle is the helper that guarantees a per-item outcome.
func TestSettleAggregatesEveryError(t *testing.T) {
	errA := errors.New("a")
	errB := errors.New("b")

	_, failures, err := SettleValues(context.Background(), []int{0, 1}, 2, func(_ context.Context, n int) (int, error) {
		if n == 0 {
			return 0, errA
		}
		return 0, errB
	})
	if err != nil {
		t.Fatalf("Settle should not report a run-level error: %v", err)
	}
	joined := errors.Join(failures...)
	if !errors.Is(joined, errA) || !errors.Is(joined, errB) {
		t.Fatalf("expected both failures to be reported, got %v", joined)
	}
}

func TestMapReturnsTheTriggeringError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Map(context.Background(), []int{0, 1, 2}, 1, func(_ context.Context, n int) (int, error) {
		if n == 0 {
			return 0, boom
		}
		return n, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
}

func TestSettleRunsEveryItemDespiteFailures(t *testing.T) {
	var executed atomic.Int32
	boom := errors.New("boom")

	items := []int{0, 1, 2, 3, 4}
	outcomes, err := Settle(context.Background(), items, 2, func(_ context.Context, n int) (int, error) {
		executed.Add(1)
		if n%2 == 0 {
			return 0, boom
		}
		return n, nil
	})
	if err != nil {
		t.Fatalf("Settle should not report an error when only items failed: %v", err)
	}
	if executed.Load() != int32(len(items)) {
		t.Fatalf("expected all %d items to run, only %d did", len(items), executed.Load())
	}
	if len(outcomes) != len(items) {
		t.Fatalf("expected %d outcomes, got %d", len(items), len(outcomes))
	}
	for i, o := range outcomes {
		if o.Index != i {
			t.Fatalf("outcome %d has index %d; outcomes must be positional", i, o.Index)
		}
		if i%2 == 0 && o.OK() {
			t.Fatalf("item %d should have failed", i)
		}
		if i%2 == 1 && !o.OK() {
			t.Fatalf("item %d should have succeeded, got %v", i, o.Err)
		}
	}
}

func TestParentCancellationStopsQueuedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var executed atomic.Int32

	items := make([]int, 200)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := Map(ctx, items, 2, func(ctx context.Context, _ int) (int, error) {
		executed.Add(1)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Millisecond):
			return 0, nil
		}
	})

	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	// The whole point of bailing out of dispatch: most work must never start.
	if executed.Load() >= int32(len(items)) {
		t.Fatalf("cancellation did not stop queued work: %d/%d items ran", executed.Load(), len(items))
	}
}

func TestAlreadyCancelledContextRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var executed atomic.Int32
	_, err := Map(ctx, []int{1, 2, 3}, 2, func(context.Context, int) (int, error) {
		executed.Add(1)
		return 0, nil
	})
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if executed.Load() != 0 {
		t.Fatalf("expected no work to run, %d items executed", executed.Load())
	}
}

func TestEmptyInputIsANoop(t *testing.T) {
	got, err := Map(context.Background(), []int{}, 4, func(context.Context, int) (int, error) {
		t.Fatal("fn must not be called for empty input")
		return 0, nil
	})
	if err != nil || got != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", got, err)
	}
}

func TestZeroLimitFallsBackToDefault(t *testing.T) {
	var (
		mu   sync.Mutex
		cur  int
		peak int
	)
	items := make([]int, 40)

	if err := ForEach(context.Background(), items, 0, func(context.Context, int) error {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peak > DefaultLimit {
		t.Fatalf("peak concurrency %d exceeded DefaultLimit %d", peak, DefaultLimit)
	}
}

func TestSettleValuesSplitsSuccessesFromFailures(t *testing.T) {
	boom := errors.New("boom")
	values, failures, err := SettleValues(context.Background(), []int{1, 2, 3, 4}, 2,
		func(_ context.Context, n int) (int, error) {
			if n%2 == 0 {
				return 0, boom
			}
			return n, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("expected 2 successful values, got %v", values)
	}
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %v", failures)
	}
}
