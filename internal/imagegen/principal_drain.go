package imagegen

import "context"

// Drain joins workers, not the user-visible terminal/lost task state.
func (m *JobManager) Drain(ctx context.Context) error {
	m.StopAll(ctx)
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
