package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/stretchr/testify/require"
)

func TestPathTranslationClientPreservesScopeAndTargets(t *testing.T) {
	policy := pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: "session & one"},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/space and comma,/日本語.png", Label: "Library 日本語", Recursive: false, CensorImages: true},
		}},
		Revision: 7,
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "/v1/workspaces/workspace-1/path-translation", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			require.Equal(t, policy.Scope.Kind, r.URL.Query().Get("kind"))
			require.Equal(t, policy.Scope.Provider, r.URL.Query().Get("provider"))
			require.Equal(t, policy.Scope.Model, r.URL.Query().Get("model"))
			require.Equal(t, policy.Scope.SessionID, r.URL.Query().Get("session_id"))
		case http.MethodPost:
			var actual pathtranslate.Policy
			require.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
			require.Equal(t, policy, actual)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(policy))
	}))
	defer srv.Close()
	client := captureClient(t, srv)
	loaded, err := client.LoadPathTranslation(t.Context(), "workspace-1", policy.Scope)
	require.NoError(t, err)
	require.Equal(t, policy, loaded)
	saved, err := client.SavePathTranslation(t.Context(), "workspace-1", policy)
	require.NoError(t, err)
	require.Equal(t, policy, saved)
	require.Equal(t, 2, requests)
}
