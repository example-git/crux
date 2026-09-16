package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func clientProviderAuthFixture() (providerauth.Snapshot, providerauth.AccountsState) {
	status := providerauth.Status{
		Owner: providerauth.Owner{ProviderID: "fixture"}, Configured: true,
		Credentials:  []providerauth.CredentialStatus{{Kind: "api-key", State: "absent"}, {Kind: "oauth", State: "refresh-only", Refreshable: true}},
		AccountState: "in-sync", ActiveAccountID: "account",
	}
	snapshot := providerauth.Snapshot{WorkspaceID: "workspace", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 3}, Providers: []providerauth.Status{status}}
	accounts := providerauth.AccountsState{
		Target: providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Owner: status.Owner, Generation: snapshot.Generation}, Status: status,
		Accounts: []providerauth.AccountSummary{{ID: "account", DisplayName: "Fixture account", Active: true, CredentialState: "refresh-only", Refreshable: true}},
	}
	return snapshot, accounts
}

func TestProviderAuthSDKExactRoutesAndSafeState(t *testing.T) {
	t.Parallel()
	snapshot, accounts := clientProviderAuthFixture()
	require.NoError(t, snapshot.Validate())
	require.NoError(t, accounts.Validate())
	for _, operation := range []string{"status", "accounts"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if operation == "status" {
					require.Equal(t, http.MethodGet, r.Method)
					require.Equal(t, "/v1/workspaces/workspace/auth", r.URL.Path)
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Empty(t, body)
					require.NoError(t, json.NewEncoder(w).Encode(snapshot))
					return
				}
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/workspaces/workspace/auth/accounts", r.URL.Path)
				var target providerauth.Target
				require.NoError(t, json.NewDecoder(r.Body).Decode(&target))
				require.Equal(t, accounts.Target, target)
				require.NoError(t, json.NewEncoder(w).Encode(accounts))
			}))
			defer srv.Close()
			c := captureClient(t, srv)
			if operation == "status" {
				got, err := c.ProviderAuthentication(t.Context(), snapshot.WorkspaceID)
				require.NoError(t, err)
				require.Equal(t, snapshot, got)
			} else {
				got, err := c.ProviderAccounts(t.Context(), snapshot.WorkspaceID, accounts.Target)
				require.NoError(t, err)
				require.Equal(t, accounts, got)
			}
		})
	}
}

func TestProviderAuthSDKRejectsMalformedAndChangedResponses(t *testing.T) {
	t.Parallel()
	snapshot, accounts := clientProviderAuthFixture()
	for _, operation := range []string{"status", "accounts"} {
		var value any = snapshot
		if operation == "accounts" {
			value = accounts
		}
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		valid := string(encoded)
		bodies := map[string]string{
			"null": `null`, "empty": `{}`, "trailing": valid + `{}`,
			"unknown":             strings.Replace(valid, `"workspace_id":`, `"unknown":true,"workspace_id":`, 1),
			"duplicate":           strings.Replace(valid, `"workspace_id":`, `"workspace_id":"workspace","workspace_id":`, 1),
			"private namespace":   strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","account_namespace":"private"`, 1),
			"secret field":        strings.Replace(valid, `"configured":true`, `"configured":true,"access_token":"secret"`, 1),
			"case alias":          strings.Replace(valid, `"configured":true`, `"configured":true,"Configured":false`, 1),
			"null optional owner": strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","has_oauth":null`, 1),
			"missing configured":  strings.Replace(valid, `"configured":true,`, ``, 1),
			"null disabled":       strings.Replace(valid, `"disabled":false`, `"disabled":null`, 1),
			"missing refreshable": strings.Replace(valid, `,"refreshable":false`, ``, 1),
			"invalid credential":  strings.Replace(valid, `"refresh-only"`, `"invalid"`, 1),
			"wrong workspace":     strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1),
			"invalid epoch":       strings.Replace(valid, strings.Repeat("a", 32), "epoch", 1),
			"oversized":           strings.Repeat(" ", proto.MaxProviderAuthResponseBytes) + valid,
		}
		if operation == "status" {
			bodies["null providers"] = strings.Replace(valid, `"providers":[`, `"providers":null,"unused":[`, 1)
		} else {
			bodies["wrong owner"] = strings.ReplaceAll(valid, `"provider_id":"fixture"`, `"provider_id":"other"`)
			bodies["wrong generation"] = strings.Replace(valid, `"sequence":3`, `"sequence":4`, 1)
			bodies["missing active"] = strings.Replace(valid, `"active":true,`, ``, 1)
			bodies["null display name"] = strings.Replace(valid, `"display_name":"Fixture account"`, `"display_name":null`, 1)
			bodies["null accounts"] = strings.Replace(valid, `"accounts":[`, `"accounts":null,"unused":[`, 1)
		}
		for name, body := range bodies {
			t.Run(operation+"/"+name, func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
				defer srv.Close()
				c := captureClient(t, srv)
				var err error
				if operation == "status" {
					_, err = c.ProviderAuthentication(t.Context(), snapshot.WorkspaceID)
				} else {
					_, err = c.ProviderAccounts(t.Context(), snapshot.WorkspaceID, accounts.Target)
				}
				require.Error(t, err)
			})
		}
	}
}

func TestProviderAuthSDKRejectsInputBeforeHTTPAndPreservesErrors(t *testing.T) {
	t.Parallel()
	snapshot, accounts := clientProviderAuthFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid or canceled input reached HTTP") }))
	defer srv.Close()
	c := captureClient(t, srv)
	for _, id := range []string{"", "workspace/other", "workspace" + `\` + "other", " workspace"} {
		_, err := c.ProviderAuthentication(t.Context(), id)
		require.Error(t, err)
	}
	_, err := c.ProviderAccounts(t.Context(), "other", accounts.Target)
	require.Error(t, err)
	_, err = c.ProviderAccounts(t.Context(), snapshot.WorkspaceID, providerauth.Target{})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.ProviderAuthentication(ctx, snapshot.WorkspaceID)
	require.ErrorIs(t, err, context.Canceled)
	_, err = c.ProviderAccounts(ctx, snapshot.WorkspaceID, accounts.Target)
	require.ErrorIs(t, err, context.Canceled)

	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: providerauth.ErrStale.Error()})
	}))
	defer rejected.Close()
	_, err = captureClient(t, rejected).ProviderAccounts(t.Context(), snapshot.WorkspaceID, accounts.Target)
	require.ErrorContains(t, err, providerauth.ErrStale.Error())

	canceled, err := NewClient(t.TempDir(), "tcp", "fixture.invalid:80")
	require.NoError(t, err)
	canceled.h.Transport = modelOverridesCanceledResponse{}
	_, err = canceled.ProviderAuthentication(t.Context(), snapshot.WorkspaceID)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderAuthCreateRetainsCollectedSourceAndExactNumbers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("AI_CLI_DIR", root)
	provider := config.ProviderConfig{ID: "fixture", Type: catalog.TypeOpenAICompat, BaseURL: "https://fixture.invalid/v1", APIKey: "synthetic-collected-key", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}
	store := config.NewTestStore(&config.Config{Options: &config.Options{DataDirectory: root}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"fixture": provider}), Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "fixture", Model: "model", ProviderOptions: map[string]any{"literal.number": json.Number("9007199254740993")}},
		config.SelectedModelTypeSmall: {Provider: "fixture", Model: "model"},
	}})
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	collected := store.Config()
	require.Same(t, collected, proposal.CollectionConfig())
	provider.APIKey = "synthetic-later-publication"
	require.NoError(t, store.ApplyEphemeralProviderState(map[string]config.ProviderConfig{"fixture": provider}, nil))
	require.NotSame(t, collected, store.Config())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime-capabilities" {
			_ = json.NewEncoder(w).Encode(proto.RemoteRuntimeCapabilities{PeerChannel: proto.PeerChannelProtocol, IncrementalState: true, Protocol: proto.RemoteRuntimeProtocol, RuntimeVersion: config.RemoteRuntimeVersion, Compiler: config.RemoteRuntimeCompiler, Principal: strings.Repeat("a", 64), MaxRequestBytes: config.MaxRemoteRuntimeBytes, MaxBundles: 64, MaxProviders: 64, WorkspaceSharing: "exclusive-certificate"})
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces", r.URL.Path)
		_ = json.NewEncoder(w).Encode(proto.Workspace{ID: "created", Authority: &config.RemoteAuthority{Mode: "client", Principal: strings.Repeat("a", 64), Revision: proposal.Revision, Digest: proposal.Digest}})
	}))
	defer server.Close()
	c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: root, Runtime: &proposal})
	require.NoError(t, err)
	require.Same(t, collected, created.Runtime.CollectionConfig())
	require.Equal(t, json.Number("9007199254740993"), created.Runtime.Models[config.SelectedModelTypeLarge].ProviderOptions["literal.number"])
	require.Equal(t, "synthetic-collected-key", created.Runtime.Credentials[0].APIKey)
	require.NotSame(t, &proposal, created.Runtime)
}
