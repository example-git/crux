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

func clientRuntimeControlState(value json.RawMessage) config.RuntimeControlState {
	models := clientAgentModelState()
	return config.RuntimeControlState{
		Scope:       config.ScopeGlobal,
		Target:      config.RuntimeControlTarget{Owner: models.Large.Owner, ControlID: "vendor.control", DescriptorDigest: "descriptor", Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: models.Large.Model.Model}},
		Binding:     providerregistry.RuntimeControlBinding{Kind: providerregistry.RuntimeControlModelOption},
		ScopedKnown: true, Scoped: config.RuntimeControlValue{Present: true, Value: value},
		Effective: config.RuntimeControlValue{Present: true, Value: value},
		Source:    config.RuntimeControlSource{Kind: "model", Key: "vendor.control"}, Models: models,
	}
}

func TestRuntimeControlSDKExactValuesAndRoutes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`false`, `0`, `""`, `9007199254740993`, `1.2500`} {
		for _, operation := range []string{"resolve", "set", "remove"} {
			t.Run(raw+"/"+operation, func(t *testing.T) {
				state := clientRuntimeControlState(json.RawMessage(raw))
				target := state.Target
				method, path := http.MethodPut, "/v1/workspaces/fixture/config/runtime-control"
				if operation == "resolve" {
					method, path, target.DescriptorDigest = http.MethodPost, path+"/resolve", ""
				} else if operation == "remove" {
					method, state.Scoped = http.MethodDelete, config.RuntimeControlValue{}
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, method, r.Method)
					require.Equal(t, path, r.URL.Path)
					var request proto.SetRuntimeControlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, &state.Scope, request.Scope)
					require.Equal(t, target, request.Target)
					if operation == "set" {
						require.True(t, config.RuntimeControlJSONEqual(json.RawMessage(raw), request.Value))
					} else {
						require.Empty(t, request.Value)
					}
					require.NoError(t, json.NewEncoder(w).Encode(state))
				}))
				defer srv.Close()
				c := captureClient(t, srv)
				var result config.RuntimeControlState
				var err error
				switch operation {
				case "resolve":
					result, err = c.RuntimeControlState(t.Context(), "fixture", state.Scope, target)
				case "set":
					result, err = c.SetRuntimeControl(t.Context(), "fixture", state.Scope, target, json.RawMessage(raw))
				case "remove":
					result, err = c.RemoveRuntimeControl(t.Context(), "fixture", state.Scope, target)
				}
				require.NoError(t, err)
				require.True(t, config.RuntimeControlJSONEqual(result.Effective.Value, json.RawMessage(raw)))
			})
		}
	}
}

func TestRuntimeControlSDKAllowsRequestDependentAbsenceOnReadAndRemove(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"resolve", "remove", "set"} {
		t.Run(operation, func(t *testing.T) {
			state := clientRuntimeControlState(json.RawMessage(`false`))
			state.Binding.FallbackMode = "if-absent"
			state.RuntimeDependent = true
			state.Scoped = config.RuntimeControlValue{}
			state.Effective = config.RuntimeControlValue{}
			state.Source = config.RuntimeControlSource{Kind: "absent"}
			if operation == "set" {
				state.Scoped = config.RuntimeControlValue{Present: true, Value: json.RawMessage(`false`)}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(state))
			}))
			defer srv.Close()
			c := captureClient(t, srv)
			var result config.RuntimeControlState
			var err error
			switch operation {
			case "resolve":
				result, err = c.RuntimeControlState(t.Context(), "fixture", state.Scope, state.Target)
			case "remove":
				result, err = c.RemoveRuntimeControl(t.Context(), "fixture", state.Scope, state.Target)
			case "set":
				result, err = c.SetRuntimeControl(t.Context(), "fixture", state.Scope, state.Target, json.RawMessage(`false`))
			}
			if operation == "set" {
				require.ErrorContains(t, err, "differs from the requested value")
				return
			}
			require.NoError(t, err)
			require.True(t, result.RuntimeDependent)
			require.False(t, result.Effective.Present)
			require.Equal(t, "absent", result.Source.Kind)
		})
	}
}

func TestRuntimeControlSDKRejectsMalformedAndChangedAcknowledgement(t *testing.T) {
	t.Parallel()
	state := clientRuntimeControlState(json.RawMessage(`9007199254740993`))
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	valid := string(encoded)
	for name, body := range map[string]string{
		"null": `null`, "empty": `{}`, "trailing": valid + ` {}`,
		"unknown":             strings.Replace(valid, `"scope":0`, `"scope":0,"unknown":true`, 1),
		"duplicate":           strings.Replace(valid, `"scope":0`, `"scope":0,"scope":0`, 1),
		"unknown scope":       strings.Replace(valid, `"scoped_known":true`, `"scoped_known":false`, 1),
		"invalid fallback":    strings.Replace(valid, `"kind":"model-option"`, `"kind":"model-option","fallback_mode":"unknown"`, 1),
		"dependent set":       strings.Replace(strings.Replace(valid, `"kind":"model-option"`, `"kind":"model-option","fallback_mode":"if-absent"`, 1), `"scope":0`, `"scope":0,"runtime_dependent":true`, 1),
		"missing presence":    strings.Replace(valid, `"present":true,`, ``, 1),
		"missing scope known": strings.Replace(valid, `"scoped_known":true,`, ``, 1),
		"wrong target":        strings.Replace(valid, `"vendor.control"`, `"vendor.other"`, 1),
		"wrong descriptor":    strings.Replace(valid, `"descriptor"`, `"replacement"`, 1),
		"precision loss":      strings.ReplaceAll(valid, `9007199254740993`, `9007199254740992`),
		"missing models":      strings.Replace(valid, `"models":`, `"missing_models":`, 1),
		"oversized":           strings.Repeat(" ", proto.MaxRuntimeControlResponseBytes) + valid,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			_, err := captureClient(t, srv).SetRuntimeControl(t.Context(), "fixture", state.Scope, state.Target, state.Effective.Value)
			require.Error(t, err)
		})
	}
}

func TestRuntimeControlSDKRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()
	state := clientRuntimeControlState(json.RawMessage(`false`))
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached HTTP") }))
	defer srv.Close()
	c := captureClient(t, srv)
	for _, value := range []string{``, `null`, `{}`, `[]`, `false true`, `"\ud800"`, `1e1001`, `"` + strings.Repeat("x", 16<<10) + `"`} {
		_, err := c.SetRuntimeControl(t.Context(), "fixture", state.Scope, state.Target, json.RawMessage(value))
		require.Error(t, err, value[:min(len(value), 100)])
	}
	target := state.Target
	target.DescriptorDigest = ""
	_, err := c.RemoveRuntimeControl(t.Context(), "fixture", state.Scope, target)
	require.Error(t, err)
	_, err = c.RuntimeControlState(t.Context(), "fixture", config.Scope(7), state.Target)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.RuntimeControlState(ctx, "fixture", state.Scope, state.Target)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRuntimeControlSDKPreservesReceiverErrorAndCancellation(t *testing.T) {
	t.Parallel()
	state := clientRuntimeControlState(json.RawMessage(`false`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "runtime control descriptor changed; resolve again"})
	}))
	defer srv.Close()
	_, err := captureClient(t, srv).RemoveRuntimeControl(t.Context(), "fixture", state.Scope, state.Target)
	require.ErrorContains(t, err, "runtime control descriptor changed; resolve again")
	c, err := NewClient(t.TempDir(), "tcp", "fixture.invalid:80")
	require.NoError(t, err)
	c.h.Transport = modelOverridesCanceledResponse{}
	_, err = c.SetRuntimeControl(t.Context(), "fixture", state.Scope, state.Target, state.Effective.Value)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRuntimeControlWorkspaceRefreshRejectsMismatchedIdentity(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(proto.Workspace{ID: "different", Authority: &config.RemoteAuthority{Mode: "client", Principal: "same", Revision: 2, Digest: "same"}})
	}))
	defer srv.Close()
	_, err := captureClient(t, srv).GetWorkspace(t.Context(), "fixture")
	require.ErrorContains(t, err, "changed identity")
}

func TestRuntimeControlSDKThroughRegisteredRoute(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		t.Setenv(name, dir)
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfileIntegrated))
	path := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(path, 0o700))
	configPath := filepath.Join(os.Getenv("CRUX_GLOBAL_DATA"), "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"codex":{"api_key":"synthetic-receiver-key","models":[{"id":"gpt-5.6","name":"Fixture","context_window":8192,"default_max_tokens":1024,"can_reason":true,"reasoning_levels":["low","medium","high"],"default_reasoning_effort":"medium"}]}},"models":{"large":{"provider":"codex","model":"gpt-5.6"},"small":{"provider":"codex","model":"gpt-5.6"}}}`), 0o600))
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(srv.Backend().Shutdown)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := captureClient(t, hs)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: path, DataDir: filepath.Join(root, "workspace-data"), AuthorityMode: "server"})
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6", created.Config.Models[config.SelectedModelTypeLarge].Model)
	owner, ok := created.Config.ProviderOwner("codex")
	require.True(t, ok)
	target := config.RuntimeControlTarget{Owner: owner, ControlID: "options.analysis_effort", Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: "gpt-5.6"}}
	state, err := c.RuntimeControlState(t.Context(), created.ID, config.ScopeGlobal, target)
	require.NoError(t, err)
	require.True(t, state.ScopedKnown)
	require.False(t, state.Scoped.Present)
	target = state.Target
	beforeAccounts := modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR"))
	state, err = c.SetRuntimeControl(t.Context(), created.ID, config.ScopeGlobal, target, json.RawMessage(`"high"`))
	require.NoError(t, err)
	require.JSONEq(t, `"high"`, string(state.Effective.Value))
	current, err := c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "high", current.Config.Options.AnalysisEffort)
	disk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var saved struct {
		Options config.Options `json:"options"`
	}
	require.NoError(t, json.Unmarshal(disk, &saved))
	require.Equal(t, "high", saved.Options.AnalysisEffort)
	stale := target
	stale.Owner.ManifestVersion += "-changed"
	_, err = c.SetRuntimeControl(t.Context(), created.ID, config.ScopeGlobal, stale, json.RawMessage(`"low"`))
	require.ErrorContains(t, err, "owner")
	stale = target
	stale.DescriptorDigest = "replaced"
	_, err = c.RemoveRuntimeControl(t.Context(), created.ID, config.ScopeGlobal, stale)
	require.ErrorContains(t, err, "descriptor")
	unchanged, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, disk, unchanged)
	state, err = c.RemoveRuntimeControl(t.Context(), created.ID, config.ScopeGlobal, target)
	require.NoError(t, err)
	require.False(t, state.Scoped.Present)
	current, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.Empty(t, current.Config.Options.AnalysisEffort)
	require.Equal(t, beforeAccounts, modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR")))
}
