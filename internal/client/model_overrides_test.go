package client

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestOverrideModelsSDKPreservesRequestAndCompleteReply(t *testing.T) {
	t.Parallel()
	complete := clientAgentModelState()
	requested := config.AgentModelState{Large: complete.Large}
	requested.Large.Model.ProviderOptions["integer"] = 42
	var got proto.ModelOverridesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/workspace/config/model-overrides", r.URL.Path)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.NoError(t, json.NewEncoder(w).Encode(complete))
	}))
	defer srv.Close()
	state, err := captureClient(t, srv).OverrideModels(t.Context(), "workspace", requested)
	require.NoError(t, err)
	require.Nil(t, got.State.Small)
	require.Equal(t, float64(42), got.State.Large.Model.ProviderOptions["integer"])
	require.Equal(t, got.State.Large, state.Large)
	require.Equal(t, complete.Small, state.Small)
}

func TestOverrideModelsSDKChecksNumericWireMeaning(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		number  json.Number
		changed bool
	}{
		{number: "1.0"},
		{number: "1e3"},
		{number: "0.50"},
		{number: "9007199254740993", changed: true},
		{number: "1.00000000000000000000000000001", changed: true},
	} {
		t.Run(string(test.number), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request proto.ModelOverridesRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.NoError(t, json.NewEncoder(w).Encode(request.State))
			}))
			defer srv.Close()
			requested := clientAgentModelState()
			requested.Large.Model.ProviderOptions["number"] = test.number
			_, err := captureClient(t, srv).OverrideModels(t.Context(), "workspace", requested)
			if test.changed {
				require.ErrorContains(t, err, "acknowledgement changed the requested large model")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOverrideModelsSDKRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached HTTP") }))
	defer srv.Close()
	c := captureClient(t, srv)
	_, err := c.OverrideModels(t.Context(), "workspace", config.AgentModelState{})
	require.ErrorContains(t, err, "agent model state is required")
	mismatch := clientAgentModelState()
	mismatch.Large.Owner.ProviderID = "replacement"
	_, err = c.OverrideModels(t.Context(), "workspace", mismatch)
	require.ErrorContains(t, err, "does not match model provider")
	invalidOptions := clientAgentModelState()
	invalidOptions.Large.Model.ProviderOptions["unsupported"] = func() {}
	_, err = c.OverrideModels(t.Context(), "workspace", invalidOptions)
	require.ErrorContains(t, err, "encode model overrides")
}

func TestOverrideModelsSDKRejectsMalformedOrChangedReply(t *testing.T) {
	t.Parallel()
	requested := clientAgentModelState()
	valid, err := json.Marshal(requested)
	require.NoError(t, err)
	for name, body := range map[string]string{
		"empty": `{}`, "null": `null`, "trailing": string(valid) + ` {}`,
		"unknown":                strings.Replace(string(valid), `"large":`, `"unknown":true,"large":`, 1),
		"changed model":          strings.Replace(string(valid), `large-model`, `replacement`, 1),
		"changed owner":          strings.Replace(string(valid), `plugin.same`, `plugin.replacement`, 1),
		"missing requested slot": `{"small":` + mustModelJSON(t, requested.Small) + `}`,
		"oversize":               `{"unknown":"` + strings.Repeat("x", 1<<20) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			_, err := captureClient(t, srv).OverrideModels(t.Context(), "workspace", requested)
			require.Error(t, err)
		})
	}
}

func mustModelJSON(t *testing.T, model *config.OwnedSelectedModel) string {
	t.Helper()
	data, err := json.Marshal(model)
	require.NoError(t, err)
	return string(data)
}

func TestOverrideModelsSDKPreservesExplicitReceiverError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "model missing-model is not available for provider same"})
	}))
	defer srv.Close()
	_, err := captureClient(t, srv).OverrideModels(t.Context(), "workspace", clientAgentModelState())
	require.ErrorContains(t, err, "status code 400")
	require.ErrorContains(t, err, "model missing-model is not available")
}

type modelOverridesCanceledResponse struct{}

func (modelOverridesCanceledResponse) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: modelOverridesCanceledResponse{}}, nil
}

func (modelOverridesCanceledResponse) Read([]byte) (int, error) { return 0, context.Canceled }
func (modelOverridesCanceledResponse) Close() error             { return nil }

func TestOverrideModelsSDKPreservesResponseReadCancellation(t *testing.T) {
	t.Parallel()
	c, err := NewClient(t.TempDir(), "tcp", "fixture.invalid:80")
	require.NoError(t, err)
	c.h.Transport = modelOverridesCanceledResponse{}
	_, err = c.OverrideModels(t.Context(), "workspace", clientAgentModelState())
	require.ErrorContains(t, err, "read model overrides response")
	require.ErrorIs(t, err, context.Canceled)
}

func TestOverrideModelsSDKThroughRegisteredServerRoute(t *testing.T) {
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
	original := []byte(`{"providers":{"fixture":{"id":"fixture","type":"openai-compat","api_key":"synthetic-receiver-key","base_url":"https://fixture.invalid/v1","models":[{"id":"initial","name":"Initial","context_window":8192,"default_max_tokens":1024},{"id":"replacement","name":"Replacement","context_window":8192,"default_max_tokens":1024}]}},"models":{"large":{"provider":"fixture","model":"initial"},"small":{"provider":"fixture","model":"initial"}}}`)
	require.NoError(t, os.WriteFile(configPath, original, 0o600))
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(srv.Backend().Shutdown)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := captureClient(t, hs)
	t.Cleanup(func() { require.NoError(t, c.RetireClient(context.Background())) })
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: path, DataDir: filepath.Join(root, "workspace-data"), AuthorityMode: "server"})
	require.NoError(t, err)
	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountFiles := modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR"))
	before := created.Config.AgentModelState()
	require.NotNil(t, before.Large)
	require.NotNil(t, before.Small)
	require.Equal(t, "initial", before.Large.Model.Model)
	selected := *before.Large
	selected.Model.Model = "replacement"
	selected.Model.MaxTokens = 2048
	selected.Model.Think = true
	requested := config.AgentModelState{Large: &selected}
	ack, err := c.OverrideModels(t.Context(), created.ID, requested)
	require.NoError(t, err)
	require.Equal(t, requested.Large, ack.Large)
	require.Equal(t, before.Small, ack.Small)
	current, err := c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ack, current.Config.AgentModelState())

	invalidSmall := *before.Small
	invalidSmall.Model.Model = "missing-model"
	_, err = c.OverrideModels(t.Context(), created.ID, config.AgentModelState{Large: before.Large, Small: &invalidSmall})
	require.ErrorContains(t, err, "missing-model")
	current, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ack, current.Config.AgentModelState(), "the invalid second slot must not revert the first")
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	require.Equal(t, accountFiles, modelOverridesAccountFiles(t, os.Getenv("AI_CLI_DIR")))
}

func modelOverridesAccountFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, root)] = string(data)
		return nil
	}))
	return files
}
