package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestProviderToolingSDKPreservesExactSelection(t *testing.T) {
	t.Parallel()
	owner := clientAgentModelState().Large.Owner
	for _, remove := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method := http.MethodPut
			if remove {
				method = http.MethodDelete
			}
			require.Equal(t, method, r.Method)
			require.Equal(t, "/v1/workspaces/fixture/config/provider-tooling", r.URL.Path)
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var request proto.ProviderToolingRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.NotNil(t, request.Scope)
			require.Equal(t, config.ScopeWorkspace, *request.Scope)
			require.Equal(t, owner, request.Owner)
			if remove {
				require.Empty(t, request.Profile)
			} else {
				require.Equal(t, config.ToolingInstructionsNative, request.Profile)
			}
			require.NoError(t, json.NewEncoder(w).Encode(proto.ProviderToolingState{Scope: config.ScopeWorkspace, Owner: owner, Profile: config.ToolingInstructionsNative}))
		}))
		c := captureClient(t, srv)
		var state proto.ProviderToolingState
		var err error
		if remove {
			state, err = c.RemoveProviderToolingInstructions(t.Context(), "fixture", config.ScopeWorkspace, owner)
		} else {
			state, err = c.SetProviderToolingInstructions(t.Context(), "fixture", config.ScopeWorkspace, owner, config.ToolingInstructionsNative)
		}
		srv.Close()
		require.NoError(t, err)
		require.Equal(t, owner, state.Owner)
		require.Equal(t, config.ToolingInstructionsNative, state.Profile)
	}
}

func TestProviderToolingSDKRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached HTTP") }))
	defer srv.Close()
	c := captureClient(t, srv)
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	_, err := c.SetProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, owner, "")
	require.ErrorContains(t, err, "profile")
	_, err = c.SetProviderToolingInstructions(t.Context(), "fixture", config.Scope(7), owner, config.ToolingInstructionsCrux)
	require.ErrorContains(t, err, "scope")
	_, err = c.RemoveProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, providerregistry.RegistrationOwner{})
	require.ErrorContains(t, err, "owner")
	owner.ManifestID = strings.Repeat("x", 16<<10)
	_, err = c.RemoveProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, owner)
	require.ErrorContains(t, err, "size limit")
}

func TestProviderToolingSDKRejectsMalformedOrChangedAcknowledgement(t *testing.T) {
	t.Parallel()
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	valid := `{"scope":0,"owner":{"provider_id":"fixture"},"profile":"crux"}`
	for name, body := range map[string]string{
		"null": `null`, "empty": `{}`, "trailing": valid + ` {}`,
		"scope omitted":   strings.Replace(valid, `"scope":0,`, ``, 1),
		"profile omitted": strings.Replace(valid, `,"profile":"crux"`, ``, 1),
		"changed scope":   strings.Replace(valid, `"scope":0`, `"scope":1`, 1),
		"changed owner":   strings.Replace(valid, `"fixture"`, `"other"`, 1),
		"changed profile": strings.Replace(valid, `"crux"`, `"native"`, 1),
		"unknown":         strings.Replace(valid, `"scope":0`, `"scope":0,"unknown":true`, 1),
		"oversize":        strings.Replace(valid, `"fixture"`, `"`+strings.Repeat("x", 16<<10)+`"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			_, err := captureClient(t, srv).SetProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, owner, config.ToolingInstructionsCrux)
			require.Error(t, err)
		})
	}
}

func TestProviderToolingSDKPreservesReceiverErrorAndCancellation(t *testing.T) {
	t.Parallel()
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "active owner for provider fixture changed"})
	}))
	defer srv.Close()
	_, err := captureClient(t, srv).RemoveProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, owner)
	require.ErrorContains(t, err, "active owner for provider fixture changed")
	c, err := NewClient(t.TempDir(), "tcp", "fixture.invalid:80")
	require.NoError(t, err)
	c.h.Transport = modelOverridesCanceledResponse{}
	_, err = c.SetProviderToolingInstructions(t.Context(), "fixture", config.ScopeGlobal, owner, config.ToolingInstructionsCrux)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderToolingSDKThroughRegisteredRoute(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		t.Setenv(name, dir)
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfileCoreOnly))
	path := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(path, 0o700))
	configPath := filepath.Join(os.Getenv("CRUX_GLOBAL_DATA"), "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"fixture":{"id":"fixture","type":"openai-compat","api_key":"synthetic-receiver-key","base_url":"https://fixture.invalid/v1","models":[{"id":"initial","name":"Initial","context_window":8192,"default_max_tokens":1024}]}},"models":{"large":{"provider":"fixture","model":"initial"},"small":{"provider":"fixture","model":"initial"}}}`), 0o600))
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(srv.Backend().Shutdown)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := captureClient(t, hs)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: path, DataDir: filepath.Join(root, "workspace-data"), AuthorityMode: "server"})
	require.NoError(t, err)
	owner, ok := created.Config.ProviderOwner("fixture")
	require.True(t, ok)
	accountsBefore := modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR"))
	ack, err := c.SetProviderToolingInstructions(t.Context(), created.ID, config.ScopeGlobal, owner, config.ToolingInstructionsCrux)
	require.NoError(t, err)
	current, err := c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.NoError(t, ack.ValidateConfig(current.Config))
	disk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var persisted struct {
		Providers map[string]config.ProviderConfig `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(disk, &persisted))
	require.Equal(t, config.ToolingInstructionsCrux, persisted.Providers[owner.ProviderID].ToolingInstructions)
	_, err = c.SetProviderToolingInstructions(t.Context(), created.ID, config.ScopeWorkspace, owner, config.ToolingInstructionsCrux)
	require.NoError(t, err)
	receiver, err := srv.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	require.True(t, receiver.Cfg.HasConfigField(config.ScopeWorkspace, "providers.fixture.tooling_instructions"))
	ack, err = c.RemoveProviderToolingInstructions(t.Context(), created.ID, config.ScopeWorkspace, owner)
	require.NoError(t, err)
	require.Equal(t, config.ToolingInstructionsCrux, ack.Profile, "removal must report the inherited global profile")
	require.False(t, receiver.Cfg.HasConfigField(config.ScopeWorkspace, "providers.fixture.tooling_instructions"))
	current, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.NoError(t, ack.ValidateConfig(current.Config))
	_, err = c.SetProviderToolingInstructions(t.Context(), created.ID, config.ScopeGlobal, owner, config.ToolingInstructionsNative)
	require.ErrorContains(t, err, "native tooling instructions")
	stale := owner
	stale.Construction = providerregistry.ConstructionGenericJSON
	_, err = c.RemoveProviderToolingInstructions(t.Context(), created.ID, config.ScopeGlobal, stale)
	require.ErrorContains(t, err, "owner")
	unchanged, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, disk, unchanged)
	ack, err = c.RemoveProviderToolingInstructions(t.Context(), created.ID, config.ScopeGlobal, owner)
	require.NoError(t, err)
	require.Empty(t, ack.Profile)
	current, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.NoError(t, ack.ValidateConfig(current.Config))
	disk, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.NotContains(t, string(disk), "tooling_instructions")
	require.Contains(t, string(disk), "synthetic-receiver-key")
	require.Equal(t, accountsBefore, modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR")))
}
