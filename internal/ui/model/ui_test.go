package model

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestCurrentModelSupportsImages(t *testing.T) {
	t.Parallel()

	t.Run("returns false when config is nil", func(t *testing.T) {
		t.Parallel()

		ui := newTestUIWithConfig(t, nil)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when coder agent is missing", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents:    map[string]config.Agent{},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when model is not found", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: config.SelectedModelTypeLarge},
			},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns true when current model supports images", func(t *testing.T) {
		t.Parallel()

		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set("test-provider", config.ProviderConfig{
			ID: "test-provider",
			Models: []catwalk.Model{
				{ID: "test-model", SupportsImages: true},
			},
		})

		cfg := &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {
					Provider: "test-provider",
					Model:    "test-model",
				},
			},
			Providers: providers,
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: config.SelectedModelTypeLarge},
			},
		}

		ui := newTestUIWithConfig(t, cfg)
		require.True(t, ui.currentModelSupportsImages())
	})
}

func TestCurrentImageExtensionsFollowSelectedProvider(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "codex", Model: "gpt-5"},
		},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents: map[string]config.Agent{
			config.AgentCoder: {Model: config.SelectedModelTypeLarge},
		},
	}
	ui := newTestUIWithConfig(t, cfg)

	require.Equal(t, []string{".gif", ".jpeg", ".jpg", ".png", ".webp"}, ui.currentImageExtensions())
	require.Equal(t, int64(25*1024*1024), ui.currentImageSourceLimit())
	require.True(t, hasAllowedImageExtension("/tmp/IMAGE.WEBP", ui.currentImageExtensions()))
	require.False(t, hasAllowedImageExtension("/tmp/image.webp.exe", ui.currentImageExtensions()))
}

func TestChatHighlightCopiedClearsMouseStateInUpdate(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.chat.mouseDown = true
	ui.chat.mouseDownItem = 2
	ui.chat.mouseDragItem = 3
	ui.chat.clickCount = 2

	_, _ = ui.Update(chatHighlightCopiedMsg{})

	require.False(t, ui.chat.mouseDown)
	require.Equal(t, -1, ui.chat.mouseDownItem)
	require.Equal(t, -1, ui.chat.mouseDragItem)
	require.Zero(t, ui.chat.clickCount)
}

func newTestUIWithConfig(t *testing.T, cfg *config.Config) *UI {
	t.Helper()

	return &UI{
		com: &common.Common{
			Workspace: &testWorkspace{cfg: cfg},
		},
	}
}

// testWorkspace is a minimal [workspace.Workspace] stub for unit tests.
type testWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (w *testWorkspace) Config() *config.Config {
	return w.cfg
}
