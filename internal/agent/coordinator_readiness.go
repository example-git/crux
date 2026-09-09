package agent

import "context"

// beginReadiness admits construction before shutdown starts. Both synchronous
// construction and its asynchronous children hold an admission until they exit,
// so CloseContext cannot close resources underneath an admitted child.
func (c *coordinator) beginReadiness(ctx context.Context, detached bool) (context.Context, func(), error) {
	c.readinessMu.Lock()
	defer c.readinessMu.Unlock()
	if c.readinessClosed {
		return nil, nil, context.Canceled
	}
	if c.readinessCtx == nil {
		c.readinessCtx, c.readinessCancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	if detached {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.readinessCtx, cancel)
	c.readinessWork.Add(1)
	return ctx, func() {
		stop()
		cancel()
		c.readinessWork.Done()
	}, nil
}

func (c *coordinator) startReadiness(ctx context.Context, build func(context.Context) error) error {
	ctx, finish, err := c.beginReadiness(ctx, true)
	if err != nil {
		return err
	}
	c.readyWg.Go(func() error {
		defer finish()
		if err := ctx.Err(); err != nil {
			return err
		}
		return build(ctx)
	})
	return nil
}

func (c *coordinator) stopReadiness() {
	c.readinessMu.Lock()
	c.readinessClosed = true
	if c.readinessCancel != nil {
		c.readinessCancel()
	}
	c.readinessMu.Unlock()
	// Admission is closed before waiting, including for children of an already
	// running build. Joining is required before closing shared resources.
	c.readinessWork.Wait()
}
