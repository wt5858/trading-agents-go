package domain_services

import (
	"context"
	"time"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
)

type modelObserverKey struct{}
type modelObserver func(entities.TurnResult) error

func reportModelProgress(ctx context.Context, partial entities.TurnResult) error {
	if observer, ok := ctx.Value(modelObserverKey{}).(modelObserver); ok {
		return observer(partial)
	}
	return nil
}

type checkpointRuntime struct {
	delegate entities.Runtime
	root     *entities.AnalysisContext
	save     func(context.Context) error
}

func (r checkpointRuntime) Execute(ctx context.Context, turn entities.Turn) (entities.TurnResult, error) {
	observer := modelObserver(func(partial entities.TurnResult) error {
		r.root.RecordPending(turn.Contract.Kind, partial)
		return r.save(ctx)
	})
	return r.delegate.Execute(context.WithValue(ctx, modelObserverKey{}, observer), turn)
}

// All saves for a run are serialized; the repository receives the whole aggregate.
func (s *EngineService) runRuntime(root *entities.AnalysisContext, delegate entities.Runtime) (entities.Runtime, func(context.Context) error) {
	if s.runs == nil {
		return delegate, func(context.Context) error { return nil }
	}
	gate := make(chan struct{}, 1)
	save := func(ctx context.Context) error {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-saveCtx.Done():
			return saveCtx.Err()
		}
		return s.runs.SaveCheckpoint(saveCtx, root)
	}
	return checkpointRuntime{delegate, root, save}, save
}
