package copilot

import (
	"context"

	"github.com/example-git/crux/internal/oauth/useragent"
)

// Headers returns the identity headers for the configured Copilot
// advertisement mode (VS Code Copilot Chat by default, Copilot CLI via
// COPILOT_ADVERTISE_MODE). Endpoints validate these against the client
// identity, so they must match a real client.
func Headers() map[string]string {
	return headersForIdentity(useragent.Copilot())
}

func HeadersForContext(ctx context.Context) (map[string]string, error) {
	identity, err := useragent.CopilotForContext(ctx)
	if err != nil {
		return nil, err
	}
	return headersForIdentity(identity), nil
}

func headersForIdentity(identity useragent.CopilotIdentity) map[string]string {
	headers := map[string]string{
		"User-Agent":             identity.UserAgent,
		"Copilot-Integration-Id": identity.IntegrationID,
	}
	if identity.EditorVersion != "" {
		headers["Editor-Version"] = identity.EditorVersion
	}
	if identity.EditorPluginVersion != "" {
		headers["Editor-Plugin-Version"] = identity.EditorPluginVersion
	}
	return headers
}
