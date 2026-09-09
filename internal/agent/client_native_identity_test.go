package agent

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A subprocess confines the fallback root pool to this fixture. The production
// Codex dialer still performs a normal WSS handshake and certificate validation;
// neither its endpoint nor its TLS verification is replaced by a test hook.
func TestClientCodexNativeIdentityWSS(t *testing.T) {
	const child = "CRUX_TEST_NATIVE_IDENTITY_WSS_CHILD"
	if os.Getenv(child) != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestClientCodexNativeIdentityWSS$", "-test.timeout=45s", "-test.v")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("WSS subprocess acceptance:\n%s", output)
		return
	}

	t.Setenv("GODEBUG", "x509usefallbackroots=1")
	t.Setenv("CODEX_VERSION", "host-version")
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "host-originator")
	t.Setenv("TERM_PROGRAM", "host-terminal")
	t.Setenv("TERM_PROGRAM_VERSION", "host-terminal-version")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	type observedRequest struct {
		headers http.Header
		frame   map[string]any
	}
	var mu sync.Mutex
	var requests []observedRequest
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/responses", r.URL.Path)
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var frame map[string]any
			if connection.ReadJSON(&frame) != nil {
				return
			}
			mu.Lock()
			requests = append(requests, observedRequest{headers: r.Header.Clone(), frame: frame})
			id := fmt.Sprintf("native_identity_response_%d", len(requests))
			mu.Unlock()
			item := map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Captured native title"}}}
			for _, event := range []map[string]any{
				{"type": "response.output_text.delta", "delta": "Captured native title"},
				{"type": "response.output_item.done", "item": item},
				{"type": "response.completed", "response": map[string]any{"id": id, "output": []any{}}},
			} {
				if connection.WriteJSON(event) != nil {
					return
				}
			}
		}
	}))
	defer host.Close()
	pool := x509.NewCertPool()
	pool.AddCert(host.Certificate())
	x509.SetFallbackRoots(pool)

	for _, name := range []string{"owner-overrides", "captured-default", "explicit-provider-headers"} {
		t.Run(name, func(t *testing.T) {
			identity := config.NativeIdentity{UserAgent: "owner_cli/2.3.4 (client-os client-release; client-arch) client-terminal/5.6", Version: "2.3.4", Originator: "owner_cli"}
			if name == "captured-default" {
				// Absence is resolved on the owner before transmission. This default-
				// shaped declaration must not be reinterpreted using host overrides.
				identity = config.NativeIdentity{UserAgent: "codex_cli_rs/0.146.0 (client-os client-release; client-arch) unknown", Version: "0.146.0", Originator: "codex_cli_rs"}
			}
			nextIdentity := config.NativeIdentity{UserAgent: "next_owner/7.8.9 (next-os next-release; next-arch) next-terminal", Version: "7.8.9", Originator: "next_owner"}
			registry, err := providerregistry.New(providerregistry.Integrated()...)
			require.NoError(t, err)
			registration, ok := registry.Lookup("codex")
			require.True(t, ok)
			largeSelection := config.SelectedModel{Provider: "codex", Model: "fixture-large", MaxTokens: 512}
			smallSelection := config.SelectedModel{Provider: "codex", Model: "fixture-small", MaxTokens: 128}
			proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
				Providers: []config.RemoteProviderDefinition{{NativeIdentity: &identity, Config: config.ProviderConfig{
					ID: "codex", Type: catalog.TypeOpenAICompat, BaseURL: strings.Replace(host.URL, "https://", "wss://", 1) + "/responses",
					Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
					Models: []catalog.Model{
						{ID: largeSelection.Model, Name: "Large", ContextWindow: 32000, DefaultMaxTokens: 512},
						{ID: smallSelection.Model, Name: "Small", ContextWindow: 32000, DefaultMaxTokens: 128},
					},
				}}},
				Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: largeSelection, config.SelectedModelTypeSmall: smallSelection},
				Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{
					ID: "captured-account", AccessToken: "synthetic-native-token", RefreshToken: "synthetic-native-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"captured-account-id"}`),
				}}},
			}
			oldRequestIdentity, nextRequestIdentity := identity, nextIdentity
			if name == "explicit-provider-headers" {
				oldRequestIdentity = config.NativeIdentity{UserAgent: "explicit-client-agent", Version: "explicit-client-version", Originator: "explicit-client-originator"}
				nextRequestIdentity = config.NativeIdentity{UserAgent: "next-explicit-agent", Version: "next-explicit-version", Originator: "next-explicit-originator"}
				proposal.Providers[0].Config.ExtraHeaders = map[string]string{"User-Agent": oldRequestIdentity.UserAgent, "version": oldRequestIdentity.Version, "originator": oldRequestIdentity.Originator}
			}
			sealClientResponsesProposal(t, &proposal)
			principal := strings.Repeat("a", 64)
			root := t.TempDir()
			store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{"CODEX_VERSION": "receiver-version", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "receiver-originator"}))
			require.NoError(t, err)
			coord := &coordinator{cfg: store, codexSessions: codexresponses.NewSessionStore()}
			defer coord.codexSessions.Close()
			build := func(snapshot config.RuntimeSnapshot) InstalledRuntime {
				large, small, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, snapshot)
				require.NoError(t, err)
				return InstalledRuntime{LargeModel: large, SmallModel: small, Snapshot: snapshot}
			}
			old := build(store.RuntimeSnapshot())
			environment := testEnv(t)
			agent := testSessionAgent(environment, old.LargeModel.Model, old.SmallModel.Model, "system").(*sessionAgent)
			consume := func(label string, runtime InstalledRuntime, want config.NativeIdentity) {
				mu.Lock()
				start := len(requests)
				mu.Unlock()
				for index, model := range []Model{runtime.LargeModel, runtime.SmallModel} {
					result, err := model.Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify captured identity")}, Headers: map[string]string{"x-session-id": fmt.Sprintf("%s-%s-%d", name, label, index), "x-request-purpose": "conversation"}})
					require.NoError(t, err)
					require.Equal(t, "Captured native title", result.Content.Text())
				}
				session, err := environment.sessions.Create(t.Context(), "")
				require.NoError(t, err)
				agent.generateTitleWithRuntime(t.Context(), session.ID, "native identity title", runtime)
				stored, err := environment.sessions.Get(t.Context(), session.ID)
				require.NoError(t, err)
				require.Equal(t, "Captured native title", stored.Title)
				mu.Lock()
				defer mu.Unlock()
				observed := requests[start:]
				require.Len(t, observed, 3, "large, small, and one successful title request")
				for index, request := range observed {
					require.Equal(t, want.UserAgent, request.headers.Get("User-Agent"), label)
					require.Equal(t, want.Version, request.headers.Get("version"), label)
					require.Equal(t, want.Originator, request.headers.Get("originator"), label)
					require.Equal(t, "Bearer synthetic-native-token", request.headers.Get("Authorization"))
					require.Equal(t, "captured-account-id", request.headers.Get("chatgpt-account-id"))
					wantModel := smallSelection.Model
					if index == 0 {
						wantModel = largeSelection.Model
					}
					require.Equal(t, wantModel, request.frame["model"], "title must use the accepted small model")
				}
			}
			consume("initial", old, oldRequestIdentity)
			proposal.Revision, proposal.Credentials[0].Generation = 2, 2
			proposal.Providers[0].NativeIdentity = &nextIdentity
			if name == "explicit-provider-headers" {
				proposal.Providers[0].Config.ExtraHeaders = map[string]string{"User-Agent": nextRequestIdentity.UserAgent, "version": nextRequestIdentity.Version, "originator": nextRequestIdentity.Originator}
			}
			sealClientResponsesProposal(t, &proposal)
			_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
			require.NoError(t, err)
			current := build(store.RuntimeSnapshot())
			agent.SetRuntime(current)
			t.Setenv("CODEX_VERSION", "changed-host-version")
			t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "changed-host-originator")
			consume("current", current, nextRequestIdentity)
			consume("retained", old, oldRequestIdentity)
		})
	}
}
