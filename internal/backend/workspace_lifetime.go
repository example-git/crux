package backend

import "context"

// All retirement callers join the same cleanup and observe its actual error.
type clientRetirement struct {
	done chan struct{}
	err  error
}

// A provisional creation belongs to its caller until aggregate workspace
// claims take over at publication. done closes only after provisional cleanup.
type workspaceCreation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// beginRetirement is called at the last-claim/removal decision while b.mu is
// held, before indexes become reusable. It closes the same gate used by runs.
func (w *Workspace) beginRetirement() {
	w.runMu.Lock()
	w.closing = true
	if w.cancel != nil {
		w.cancel()
	}
	w.runMu.Unlock()
}

// beginCredentialOperation preserves request cancellation and adds workspace
// expiry. Its ticket prevents storage cleanup from passing an admitted RPC.
func (w *Workspace) beginCredentialOperation(ctx context.Context) (context.Context, func(), error) {
	w.runMu.Lock()
	if w.closing {
		w.runMu.Unlock()
		return nil, nil, ErrWorkspaceClosing
	}
	if err := ctx.Err(); err != nil {
		w.runMu.Unlock()
		return nil, nil, err
	}
	w.runWG.Add(1)
	w.runMu.Unlock()
	bound, cancel := context.WithCancel(ctx)
	stop := func() bool { return false }
	if w.ctx != nil {
		stop = context.AfterFunc(w.ctx, cancel)
	}
	return bound, func() { stop(); cancel(); w.runWG.Done() }, nil
}
