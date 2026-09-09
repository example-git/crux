package agent

import (
	"context"
	"sync"
)

// auxiliaryWork owns model calls which are not foreground dispatches. In
// particular, a title may survive its prompt's cancellation, but it must not
// survive coordinator shutdown or outlive the session resources it updates.
type auxiliaryWork struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	active sync.WaitGroup
}

func (w *auxiliaryWork) begin(ctx context.Context) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if w.ctx == nil {
		w.ctx, w.cancel = context.WithCancel(context.Background())
	}
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	w.active.Add(1)
	return work, func() {
		stop()
		cancel()
		w.active.Done()
	}, nil
}

func (w *auxiliaryWork) close() {
	w.mu.Lock()
	w.closed = true
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	// Admission closes under the same lock as Add, so an auxiliary child
	// cannot appear after the shutdown join has observed an empty group.
	w.active.Wait()
}

func (c *coordinator) beginAuxiliaryWork(ctx context.Context) (context.Context, func(), error) {
	ctx, releaseRuntime := c.cfg.BindRuntimeContext(ctx)
	work, finish, err := c.auxiliary.begin(ctx)
	if err != nil {
		releaseRuntime()
		return nil, nil, err
	}
	return work, func() {
		defer finish()
		releaseRuntime()
	}, nil
}

func (a *sessionAgent) beginAuxiliaryWork(ctx context.Context) (context.Context, func(), error) {
	if a.beginAuxiliary != nil {
		return a.beginAuxiliary(ctx)
	}
	// Standalone session agents have no coordinator-owned resources. Their
	// synchronous callers retain the supplied lifetime.
	return ctx, func() {}, ctx.Err()
}
