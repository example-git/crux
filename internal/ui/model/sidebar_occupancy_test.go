package model

import (
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

func TestSidebarUsesUpdatedCompactionOccupancy(t *testing.T) {
	ui := newTestUI()
	ui.dialog = dialog.NewOverlay()
	ui.com.Workspace = &testWorkspace{cfg: &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}}
	ui.agentReady = true
	ui.agentModel.CatalogModel.ContextWindow = 200000
	ui.session = &session.Session{ID: "session"}

	for _, step := range []struct {
		session session.Session
		want    string
	}{
		{session.Session{ID: "session", PromptTokens: 100000, CompletionTokens: 1000, UnseenLocalTokens: 35000}, "~68% (136K)"},
		{session.Session{ID: "session", PromptTokens: 115000}, "57% (115K)"},
		{session.Session{ID: "session", PromptTokens: 4000, EstimatedUsage: true, SummaryMessageID: "checkpoint"}, "~2% (4K)"},
		{session.Session{ID: "session", PromptTokens: 100000}, "50% (100K)"},
	} {
		_, _ = ui.Update(pubsub.Event[session.Session]{Type: pubsub.UpdatedEvent, Payload: step.session})
		require.Equal(t, step.session.ContextTokens(), ui.session.ContextTokens())
		rendered := stripANSI(ui.modelInfo(80))
		require.Contains(t, rendered, step.want)
		if !step.session.ContextEstimated() {
			require.NotContains(t, rendered, "~")
		}
	}
}
