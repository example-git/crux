package agent

import "context"

func (m *BackgroundAgentManager) Drain(ctx context.Context) error {
	m.StopAll(ctx)
	m.mu.RLock()
	tasks := make([]*BackgroundAgentTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		tasks = append(tasks, task)
	}
	m.mu.RUnlock()
	for _, task := range tasks {
		select {
		case <-task.executionDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *coordinator) DrainCredentialWork(ctx context.Context) error {
	c.CancelAll()
	c.stopReadiness()
	if c.backgroundAgents != nil {
		if err := c.backgroundAgents.Drain(ctx); err != nil {
			return err
		}
	}
	if c.backgroundImages != nil {
		if err := c.backgroundImages.Drain(ctx); err != nil {
			return err
		}
	}
	// Close the coordinator's other credential consumers with this same join
	// context rather than the ordinary application's bounded shutdown context.
	c.CloseContext(ctx)
	return nil
}
