// Package concurrency provides the only sanctioned way to fan work out in this service.
//
// Call sites must never spawn raw goroutines or use errgroup directly: unbounded fan-out
// is how a single slow dependency turns into thousands of in-flight requests, and ad-hoc
// WaitGroup code routinely drops errors and leaks goroutines on cancellation.
//
// Every helper here guarantees four things:
//   - bounded parallelism (an explicit ceiling, never len(items) goroutines)
//   - context propagation (each unit of work receives a derived context)
//   - error aggregation (nothing is silently swallowed)
//   - prompt cancellation (parent cancellation stops queued work immediately)
//   - panic containment (a panicking item fails that item, not the process — see invoke)
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// DefaultLimit is the fallback ceiling when a caller passes limit <= 0.
// Deliberately small: most fan-out here lands on a rate-limited third party,
// and an accidental zero should degrade to "slow but safe", not "hammer the dependency".
const DefaultLimit = 8

// Outcome is the per-item result of a Settle run.
type Outcome[R any] struct {
	Index int
	Value R
	Err   error
}

func (o Outcome[R]) OK() bool { return o.Err == nil }

// ForEach runs fn over items with bounded parallelism, failing fast.
//
// The first error cancels the shared context so in-flight and queued work stops promptly.
// Errors observed before the cancellation propagates are joined together, but this is
// explicitly not "every item's error": peers are cancelled, and undispatched items never
// run at all. Use Settle when you need an outcome for every item.
func ForEach[T any](ctx context.Context, items []T, limit int, fn func(context.Context, T) error) error {
	_, err := Map(ctx, items, limit, func(ctx context.Context, item T) (struct{}, error) {
		return struct{}{}, fn(ctx, item)
	})
	return err
}

// Map runs fn over items with bounded parallelism and returns results positionally,
// failing fast on the first error.
//
// Results keep the input order regardless of completion order — callers routinely zip
// them back against the input slice, and completion-ordered results would corrupt that
// silently rather than loudly.
//
// On failure the returned error wraps the triggering error; sibling items typically
// surface as context.Canceled rather than their own failures, which is the intended
// trade-off of failing fast.
func Map[T, R any](ctx context.Context, items []T, limit int, fn func(context.Context, T) (R, error)) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]R, len(items))
	outcomes := run(runCtx, items, limit, fn, func() { cancel() })

	var errs []error
	for _, o := range outcomes {
		if o.Err != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", o.Index, o.Err))
			continue
		}
		results[o.Index] = o.Value
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	// Distinguish "a worker failed" from "the caller's context died": with no per-item
	// error to report, an aborted run would otherwise look like a successful empty result.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Settle runs fn over every item with bounded parallelism and reports each outcome
// independently. It does not cancel on failure.
//
// This is for tolerant fan-out — the analyst stage, for instance, where one dead data
// source must not abort the other five. Use Map when any failure invalidates the whole
// operation.
//
// The returned error is non-nil only when the parent context was cancelled; per-item
// failures live in the outcomes.
func Settle[T, R any](ctx context.Context, items []T, limit int, fn func(context.Context, T) (R, error)) ([]Outcome[R], error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	outcomes := run(ctx, items, limit, fn, nil)

	// 先把每个槽位填成「没派发出去」，再用真实结果覆盖。
	//
	// 不预填的话，被取消打断的那次运行会留下一批零值 Outcome：Err 是 nil、
	// Value 是零值，于是 OK() 对**从未执行过**的工作返回 true。今天所有调用方
	// 都先检查返回的 err 再碰 outcomes，所以没坏；但这是个安静的陷阱，
	// 而它恰好藏在一个以「什么都不会被静默吞掉」为卖点的包里。
	notRun := context.Cause(ctx)
	if notRun == nil {
		notRun = errors.New("concurrency: 该任务未被派发")
	}
	ordered := make([]Outcome[R], len(items))
	for i := range ordered {
		ordered[i] = Outcome[R]{Index: i, Err: notRun}
	}
	for _, o := range outcomes {
		ordered[o.Index] = o
	}
	if err := ctx.Err(); err != nil {
		return ordered, err
	}
	return ordered, nil
}

// SettleValues is Settle reduced to the successful values plus the failures, for callers
// that only need "what worked and what didn't" without the positional mapping.
func SettleValues[T, R any](ctx context.Context, items []T, limit int, fn func(context.Context, T) (R, error)) (values []R, failures []error, err error) {
	outcomes, err := Settle(ctx, items, limit, fn)
	for _, o := range outcomes {
		if o.Err != nil {
			failures = append(failures, fmt.Errorf("item %d: %w", o.Index, o.Err))
			continue
		}
		values = append(values, o.Value)
	}
	return values, failures, err
}

// run is the shared worker-pool core.
//
// A fixed pool draining a channel is used rather than "one goroutine per item gated by a
// semaphore": with a semaphore, N goroutines are still created up front, so a 50k-item
// slice allocates 50k stacks before any throttling takes effect.
func run[T, R any](
	ctx context.Context,
	items []T,
	limit int,
	fn func(context.Context, T) (R, error),
	onError func(),
) []Outcome[R] {
	workers := normalizeLimit(limit, len(items))

	type job struct {
		index int
		item  T
	}
	jobs := make(chan job)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes = make([]Outcome[R], 0, len(items))
		failOnce sync.Once
	)

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				// Re-check between items so a cancelled run drains the queue
				// instead of executing the remaining work.
				if err := ctx.Err(); err != nil {
					mu.Lock()
					outcomes = append(outcomes, Outcome[R]{Index: j.index, Err: err})
					mu.Unlock()
					continue
				}

				value, err := invoke(ctx, fn, j.item)

				mu.Lock()
				outcomes = append(outcomes, Outcome[R]{Index: j.index, Value: value, Err: err})
				mu.Unlock()

				if err != nil && onError != nil {
					failOnce.Do(onError)
				}
			}
		}()
	}

	// Feed the pool. Abandon dispatch on cancellation so a cancelled run does not block
	// waiting for workers that are themselves shutting down.
producer:
	for i, item := range items {
		select {
		case jobs <- job{index: i, item: item}:
		case <-ctx.Done():
			break producer
		}
	}
	close(jobs)
	wg.Wait()

	return outcomes
}

// invoke runs one unit of work and turns a panic into that item's error.
//
// The panic has to be caught here, and nowhere else. A worker goroutine's panic is not
// the caller's panic: it unwinds a stack the caller cannot see and takes the whole
// process with it. The AMQP layer and the domain-event bus each recover on their own
// goroutine, but every consumer path that fans out — the analyst stage, the tool round,
// the market-data sync — escapes that protection the moment it enters this pool. Without
// this recover, one nil-map write in a single analyst kills the worker process and drops
// every other in-flight, unacked delivery along with it.
//
// Containing it as an Outcome error is the right blast radius: Settle already treats a
// failed item as a normal outcome (that is its entire purpose), and Map already fails
// fast on the first error. A panicking item is just an item that failed.
//
// The panic value goes into the error rather than being logged and swallowed — callers
// already report per-item errors, and a swallowed panic is how a reproducible crash turns
// into an unreproducible "sometimes the analysis comes back empty".
func invoke[T, R any](ctx context.Context, fn func(context.Context, T) (R, error), item T) (value R, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero R
			value = zero
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return fn(ctx, item)
}

// normalizeLimit clamps the worker count to something sane: never more workers than
// items (idle goroutines are pure overhead), never fewer than one (zero would deadlock).
func normalizeLimit(limit, itemCount int) int {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > itemCount {
		limit = itemCount
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}
