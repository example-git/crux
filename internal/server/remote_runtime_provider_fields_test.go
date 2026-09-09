package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRemoteRuntimeProviderFieldsDecode(t *testing.T) {
	for _, requestType := range []string{"create", "update"} {
		t.Run(requestType, func(t *testing.T) {
			for _, test := range []struct {
				name string
				data string
				err  string
			}{
				{"omitted profile", `{"runtime":{"providers":[{"config":{}}]}}`, ""},
				{"crux profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":"crux"}}]}}`, ""},
				{"native profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":"native"}}]}}`, ""},
				{"empty profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":""}}]}}`, "must be crux or native"},
				{"null profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":null}}]}}`, "must be crux or native"},
				{"unknown profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":"other"}}]}}`, "must be crux or native"},
				{"non-string profile", `{"runtime":{"providers":[{"config":{"tooling_instructions":[]}}]}}`, "must be crux or native"},
				{"cased path", `{"RUNTIME":{"PROVIDERS":[{"CONFIG":{"TOOLING_INSTRUCTIONS":null}}]}}`, "must be crux or native"},
				{"escaped path", `{"runtime":{"providers":[{"config":{"tooling_\u0069nstructions":""}}]}}`, "must be crux or native"},
				{"empty context", `{"runtime":{"provider_context_instructions":{"p":""}}}`, ""},
				{"null context map", `{"runtime":{"provider_context_instructions":null}}`, ""},
				{"null context entry", `{"runtime":{"provider_context_instructions":{"p":null}}}`, "must be Unicode strings"},
				{"non-string context", `{"runtime":{"provider_context_instructions":{"p":12}}}`, "must be Unicode strings"},
				{"high surrogate", `{"runtime":{"provider_context_instructions":{"p":"\ud800"}}}`, "must be Unicode strings"},
				{"low surrogate", `{"runtime":{"provider_context_instructions":{"p":"\udc00"}}}`, "must be Unicode strings"},
				{"unpaired high surrogate", `{"runtime":{"provider_context_instructions":{"p":"\ud800\u0061"}}}`, "must be Unicode strings"},
				{"surrogate pair", `{"runtime":{"provider_context_instructions":{"p":"\ud83d\ude80"}}}`, ""},
				{"escaped slash", `{"runtime":{"provider_context_instructions":{"p":"\\ud800"}}}`, ""},
				{"literal replacement", `{"runtime":{"provider_context_instructions":{"p":"�"}}}`, ""},
				{"escaped replacement", `{"runtime":{"provider_context_instructions":{"p":"\ufffd"}}}`, ""},
				{"invalid UTF-8", "{\"runtime\":{\"provider_context_instructions\":{\"p\":\"\xff\"}}}", "must be valid UTF-8"},
				{"plugin configuration keys", `{"runtime":{"providers":[{"config":{"configuration":{"tooling_instructions":null,"provider_context_instructions":{"p":null},"nested":{"tooling_instructions":""}}}}]}}`, ""},
			} {
				t.Run(test.name, func(t *testing.T) {
					var result any = &proto.CreateWorkspaceRequest{}
					if requestType == "update" {
						result = &proto.UpdateRemoteRuntimeRequest{}
					}
					r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.data))
					err := decodeRuntimeRequest(httptest.NewRecorder(), r, result)
					if test.err == "" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, test.err)
					}
				})
			}
		})
	}
}

func TestRemoteRuntimeProviderFieldsTLSAdmission(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	_ = tools.NewBashTool(nil, nil, t.TempDir())
	project := filepath.Join(os.Getenv("HOME"), "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	project, err := filepath.EvalSymlinks(project)
	require.NoError(t, err)
	serverState := remoteServerState(t)
	owner := providerregistry.RegistrationOwner{ProviderID: "client-instructions"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
			ID: owner.ProviderID, Name: "Client", Type: catalog.TypeOpenAICompat, BaseURL: "https://client.invalid/v1",
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "client-model", Name: "Client model", ContextWindow: 8192, DefaultMaxTokens: 1024}},
		}}},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "client-model"},
			config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "client-model"},
		},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-private-key"}},
	}
	seal := func(text string, revision uint64) config.RemoteRuntimeProposal {
		t.Helper()
		candidate := proposal
		candidate.Revision = revision
		candidate.ProviderContextInstructions = map[string]string{owner.ProviderID: text}
		candidate.Digest, err = config.RemoteRuntimeDigest(candidate)
		require.NoError(t, err)
		return candidate
	}
	marshal := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return data
	}
	call := func(method, route string, data []byte) (int, []byte) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, hs.URL+route, bytes.NewReader(data))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Crux-Runtime-Protocol", proto.RemoteRuntimeProtocol)
		r.Header.Set(cruxlog.EphemeralStateHeader, "1")
		response, err := clients["retained"].Do(r)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "synthetic-private")
		return response.StatusCode, body
	}
	cases := []struct {
		name    string
		context string
		from    string
		to      string
		err     string
	}{
		{"invalid UTF-8", "private-context-�", `"private-context-�"`, "\"private-context-\xff\"", "must be valid UTF-8"},
		{"high surrogate", "private-context-�", `"private-context-�"`, `"private-context-\ud800"`, "must be Unicode strings"},
		{"low surrogate", "private-context-�", `"private-context-�"`, `"private-context-\udc00"`, "must be Unicode strings"},
		{"null context", "", `"provider_context_instructions":{"client-instructions":""}`, `"provider_context_instructions":{"client-instructions":null}`, "must be Unicode strings"},
		{"empty profile", "", `"config":{`, `"config":{"tooling_instructions":"",`, "must be crux or native"},
		{"null profile", "", `"config":{`, `"config":{"tooling_instructions":null,`, "must be crux or native"},
	}
	clientID := uuid.NewString()
	create := func(candidate *config.RemoteRuntimeProposal) proto.CreateWorkspaceRequest {
		return proto.CreateWorkspaceRequest{Workspace: proto.Workspace{Path: project, ClientID: clientID}, AuthorityMode: "client", Runtime: candidate}
	}
	for _, test := range cases {
		t.Run("create/"+test.name, func(t *testing.T) {
			candidate := seal(test.context, 1)
			data := marshal(create(&candidate))
			require.Equal(t, 1, bytes.Count(data, []byte(test.from)))
			data = bytes.Replace(data, []byte(test.from), []byte(test.to), 1)
			// The lossy Go decode has the sealed digest, so digest validation
			// alone cannot reject these malformed representations.
			var lossy proto.CreateWorkspaceRequest
			require.NoError(t, json.Unmarshal(data, &lossy))
			digest, err := config.RemoteRuntimeDigest(*lossy.Runtime)
			require.NoError(t, err)
			require.Equal(t, candidate.Digest, digest)
			status, body := call(http.MethodPost, "/v1/workspaces", data)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			require.Contains(t, string(body), "failed to decode request")
			require.Equal(t, serverState, remoteServerState(t), "rejected creation must not write receiver files")
		})
	}
	status, body := call(http.MethodGet, "/v1/workspaces", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.JSONEq(t, "[]", string(body))
	initial := seal("private-context-�", 1)
	status, body = call(http.MethodPost, "/v1/workspaces", marshal(create(&initial)))
	require.Equal(t, http.StatusOK, status, string(body))
	var workspace proto.Workspace
	require.NoError(t, json.Unmarshal(body, &workspace))
	require.NotNil(t, workspace.Authority)
	require.Equal(t, uint64(1), workspace.Authority.Revision)
	for _, test := range cases {
		t.Run("replace/"+test.name, func(t *testing.T) {
			candidate := seal(test.context, 2)
			data := marshal(proto.UpdateRemoteRuntimeRequest{ExpectedRevision: 1, Runtime: candidate})
			require.Equal(t, 1, bytes.Count(data, []byte(test.from)))
			data = bytes.Replace(data, []byte(test.from), []byte(test.to), 1)
			status, body := call(http.MethodPut, "/v1/workspaces/"+workspace.ID+"/runtime", data)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			require.Contains(t, string(body), test.err)
			status, body = call(http.MethodGet, "/v1/workspaces/"+workspace.ID, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var current proto.Workspace
			require.NoError(t, json.Unmarshal(body, &current))
			require.Equal(t, workspace.Authority, current.Authority)
		})
	}
	updated := seal("private-context-🚀", 2)
	updated.Providers[0].Config.ToolingInstructions = config.ToolingInstructionsCrux
	updated.Digest, err = config.RemoteRuntimeDigest(updated)
	require.NoError(t, err)
	data := marshal(proto.UpdateRemoteRuntimeRequest{ExpectedRevision: 1, Runtime: updated})
	data = bytes.Replace(data, []byte("🚀"), []byte(`\ud83d\ude80`), 1)
	status, body = call(http.MethodPut, "/v1/workspaces/"+workspace.ID+"/runtime", data)
	require.Equal(t, http.StatusOK, status, string(body))
	var ack config.RemoteAuthority
	require.NoError(t, json.Unmarshal(body, &ack))
	require.Equal(t, uint64(2), ack.Revision)
	status, body = call(http.MethodGet, "/v1/workspaces/"+workspace.ID, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &workspace))
	provider, ok := workspace.Config.Providers.Get(owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, config.ToolingInstructionsCrux, provider.ToolingInstructions)
}
