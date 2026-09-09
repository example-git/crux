package app

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/agent/tools/mcp"
)

// DrainCredentialWork closes admission and joins credential-bearing work.
// Callers that need an acknowledgement must inspect the returned error.
func (app *App) DrainCredentialWork(ctx context.Context) error {
	app.agentInitMu.Lock()
	app.agentClosing = true
	coordinators := append(app.retiredCoordinators[:0:0], app.retiredCoordinators...)
	if app.AgentCoordinator != nil {
		coordinators = append(coordinators, app.AgentCoordinator)
	}
	app.agentInitMu.Unlock()
	var result error
	for _, coordinator := range coordinators {
		coordinator.CancelAll()
		if drainer, ok := coordinator.(interface{ DrainCredentialWork(context.Context) error }); ok {
			result = errors.Join(result, drainer.DrainCredentialWork(ctx))
		}
	}
	if app.BackgroundAgents != nil {
		result = errors.Join(result, app.BackgroundAgents.Drain(ctx))
	}
	if app.BackgroundShells != nil {
		result = errors.Join(result, app.BackgroundShells.Drain(ctx))
	}
	if app.BackgroundImages != nil {
		result = errors.Join(result, app.BackgroundImages.Drain(ctx))
	}
	result = errors.Join(result, mcp.For(app.config).Close(ctx))
	return result
}
