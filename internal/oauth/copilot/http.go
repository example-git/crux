package copilot

import (
	"github.com/example-git/crux/internal/oauth/useragent"
)

// Headers returns the identity headers for the configured Copilot
// advertisement mode (VS Code Copilot Chat by default, Copilot CLI via
// COPILOT_ADVERTISE_MODE). Endpoints validate these against the client
// identity, so they must match a real client.
func Headers() map[string]string {
	id := useragent.Copilot()
	headers := map[string]string{
		"User-Agent":             id.UserAgent,
		"Copilot-Integration-Id": id.IntegrationID,
	}
	if id.EditorVersion != "" {
		headers["Editor-Version"] = id.EditorVersion
	}
	if id.EditorPluginVersion != "" {
		headers["Editor-Plugin-Version"] = id.EditorPluginVersion
	}
	return headers
}
