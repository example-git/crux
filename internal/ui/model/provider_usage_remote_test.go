package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderUsageUIUsesRemoteSurfaceWithoutCredentials(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "remote-only", Construction: providerregistry.ConstructionGenericJSON}
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		assert.Equal(t, "/v1/workspaces/fixture/providers/usage", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		var request config.ProviderUsageRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, owner, request.Owner)
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(config.ProviderUsageResult{Usage: &oauthusage.Usage{ProviderID: owner.ProviderID, Windows: []oauthusage.Window{{Name: "weekly", Percent: 75}}}})
	}))
	defer remote.Close()
	c, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(remote.URL, "http://"))
	require.NoError(t, err)
	ui := newTestUI()
	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	cfg.Providers.Set(owner.ProviderID, config.ProviderConfig{ID: owner.ProviderID})
	remoteWorkspace := workspace.NewClientWorkspace(c, proto.Workspace{ID: "fixture", Config: cfg,
		ProviderSurfaces: []providerregistry.Surface{{ID: owner.ProviderID, Owner: &owner, Available: true, UsageAvailable: true}},
	})
	defer remoteWorkspace.Shutdown()
	ui.com.Workspace = remoteWorkspace
	_, registered := cfg.ProviderRegistration(owner.ProviderID)
	require.False(t, registered)
	provider, _ := cfg.Providers.Get(owner.ProviderID)
	require.Nil(t, provider.OAuthToken)
	command := ui.fetchProviderUsageFor(owner.ProviderID)
	require.Zero(t, calls.Load(), "preparing a UI command must perform no network I/O")
	msg, ok := command().(usageUpdatedMsg)
	require.True(t, ok)
	require.NotNil(t, msg.usage)
	_, _ = ui.Update(msg)
	require.Contains(t, ui.usageBars(40, true), "25% left")
	require.EqualValues(t, 1, calls.Load())
}
