package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type authenticationMutationFixture struct {
	store         *ConfigStore
	owner         providerregistry.RegistrationOwner
	first, second accounts.Entry
	root, path    string
	scope         Scope
}

func newAuthenticationMutationFixture(t *testing.T, scope Scope, disabled bool) authenticationMutationFixture {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileIntegrated), "CRUX_DISABLE_AUTO_MEMORY": "true"}
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	for _, dir := range []string{"config", "data", "workspace-data"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	first := accounts.Entry{ID: "first", DisplayName: "First", AccessToken: "synthetic-first-access", RefreshToken: "synthetic-first-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"synthetic-account-a"}`)}
	second := accounts.Entry{ID: "second", DisplayName: "Second", AccessToken: "synthetic-second-access", RefreshToken: "synthetic-second-refresh", ExpiresAt: first.ExpiresAt, Raw: json.RawMessage(`{"account_id":"synthetic-account-b"}`)}
	// The integrated registry owns this exact namespace; account saves precede
	// loading so normal loader adoption is also part of the fixture.
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), registration.AccountNamespace, second))
	source := fmt.Sprintf(`{"providers":{"codex":{"disable":%t,"base_url":"https://example.invalid/v1","models":[{"id":"main","default_max_tokens":100},{"id":"small","default_max_tokens":50}],"extra_headers":{"X-Keep":"accepted"},"provider_options":{"keep":false}},"unrelated":{"type":"openai-compat","base_url":"https://other.example.invalid/v1","api_key":"synthetic-unrelated","models":[{"id":"other"}]}},"models":{"large":{"provider":"codex","model":"main","max_tokens":71},"small":{"provider":"codex","model":"small","max_tokens":33}}}`, false)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(source), 0o600))
	path := filepath.Join(root, "workspace-data", "crux.json")
	if scope == ScopeGlobal {
		path = filepath.Join(root, "data", "crux.json")
	}
	data, err := json.Marshal(map[string]any{"providers": map[string]any{"codex": map[string]any{"api_key": first.AccessToken, "oauth": first.Token()}}, "kept_unknown": json.RawMessage(`{"integer":9007199254740993}`)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	if disabled {
		require.NoError(t, store.SetProviderDisabled(scope, owner, true))
	}
	return authenticationMutationFixture{store: store, owner: owner, first: first, second: second, root: root, path: path, scope: scope}
}

func (f authenticationMutationFixture) capture(t *testing.T) AuthenticationCapture {
	t.Helper()
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	return before
}

func TestAuthenticationMutationSwitchLogoutScopedAndSamePointer(t *testing.T) {
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("scope-%d-disabled-%t", scope, disabled), func(t *testing.T) {
				f := newAuthenticationMutationFixture(t, scope, disabled)
				before := f.capture(t)
				old := before.runtime.Config()
				models, agents := old.Models, old.Agents
				var prepared *Config
				var committed, aborted int
				f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					prepared = snapshot.Config()
					active, err := accounts.Active(ctx, f.owner.AccountNamespace)
					require.NoError(t, err)
					require.NotNil(t, active)
					require.Equal(t, "first", active.ID, "runtime preparation must precede selection")
					entry, captured, err := snapshot.CapturedConstructionAccount(f.owner)
					require.NoError(t, err)
					require.True(t, captured)
					require.Equal(t, "second", entry.ID)
					return RuntimeGenerationCandidate{Commit: func() { committed++ }, Abort: func() { aborted++ }}, nil
				})
				result, err := f.store.SwitchAuthenticationAccount(t.Context(), scope, before, f.owner, "second")
				require.NoError(t, err)
				require.False(t, result.AccountRefreshed)
				require.True(t, result.AccountsSaved)
				require.True(t, result.ConfigSaved)
				require.True(t, result.RuntimePublished)
				require.Equal(t, 1, committed)
				require.Zero(t, aborted)
				require.Same(t, prepared, f.store.Config())
				require.Equal(t, models, f.store.Config().Models)
				require.Equal(t, agents, f.store.Config().Agents)
				provider, _ := f.store.Config().Providers.Get("codex")
				require.Equal(t, disabled, provider.Disable)
				require.Equal(t, f.second.AccessToken, provider.APIKey)
				require.Empty(t, provider.APIKeyTemplate)
				oldProvider, _ := old.Providers.Get("codex")
				require.Equal(t, f.first.AccessToken, oldProvider.APIKey)
				active, err := accounts.Active(t.Context(), f.owner.AccountNamespace)
				require.NoError(t, err)
				require.Equal(t, "second", active.ID)
				after := f.capture(t)
				require.True(t, result.After.SameObservation(after))
				runtime, ok := result.RuntimeSnapshot()
				require.True(t, ok)
				require.True(t, runtime.SamePublication(f.store.RuntimeSnapshot()))
				for _, value := range []any{result, &result} {
					_, err := json.Marshal(value)
					require.Error(t, err)
					require.NotContains(t, fmt.Sprintf("%#v", value), f.second.AccessToken)
				}
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Contains(t, string(data), "9007199254740993")
				f.store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					prepared = snapshot.Config()
					require.ErrorIs(t, snapshot.AuthenticationRevocation("codex"), ErrAuthenticationRevoked)
					entry, captured, err := snapshot.CapturedConstructionAccount(f.owner)
					require.NoError(t, err)
					require.True(t, captured)
					require.Nil(t, entry)
					return RuntimeGenerationCandidate{Commit: func() { committed++ }, Abort: func() { aborted++ }}, nil
				})
				loggedOut, err := f.store.LogoutAuthentication(t.Context(), scope, after, f.owner)
				require.NoError(t, err)
				require.True(t, loggedOut.AccountsSaved)
				require.True(t, loggedOut.ConfigSaved)
				require.True(t, loggedOut.RuntimePublished)
				require.Same(t, prepared, f.store.Config())
				require.Equal(t, 2, committed)
				require.Zero(t, aborted)
				require.True(t, loggedOut.After.SameObservation(f.capture(t)))
				active, err = accounts.Active(t.Context(), f.owner.AccountNamespace)
				require.NoError(t, err)
				require.Nil(t, active)
				provider, _ = f.store.Config().Providers.Get("codex")
				require.Empty(t, provider.APIKey)
				require.Nil(t, provider.OAuthToken)
				require.Equal(t, disabled, provider.Disable)
				require.Equal(t, models, f.store.Config().Models)
				require.Equal(t, agents, f.store.Config().Agents)
				require.ErrorIs(t, f.store.RuntimeSnapshot().AuthenticationRevocation("codex"), ErrAuthenticationRevoked)
				data, err = os.ReadFile(f.path)
				require.NoError(t, err)
				require.NotContains(t, string(data), f.first.AccessToken)
				require.NotContains(t, string(data), f.second.AccessToken)
				require.Contains(t, string(data), "9007199254740993")
			})
		}
	}
}

func TestAuthenticationMutationRejectsBeforeDurableChanges(t *testing.T) {
	for _, failure := range []string{"stale publication", "foreign file", "fresh unaccepted edit", "same bytes replacement", "account replacement", "preparer error", "preparer cancellation", "empty access", "invalid scope", "wrong owner"} {
		t.Run(failure, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			before := f.capture(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			scope, owner := f.scope, f.owner
			switch failure {
			case "stale publication":
				f.store.setConfig(f.store.Config())
			case "foreign file":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "fresh unaccepted edit":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				data, err = runtimeControlChangeField(data, []string{"options", "disable_auto_summarize"}, json.RawMessage(`true`), false)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, data, 0o600))
				before = f.capture(t)
			case "same bytes replacement":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path+".peer", data, 0o600))
				require.NoError(t, os.Rename(f.path+".peer", f.path))
			case "account replacement":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, f.second))
			case "preparer error":
				f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					return RuntimeGenerationCandidate{}, errors.New("fixture preparation failed")
				})
			case "preparer cancellation":
				f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					cancel()
					return RuntimeGenerationCandidate{Commit: func() { t.Error("canceled candidate committed") }, Abort: func() {}}, nil
				})
			case "empty access":
				entry := f.second
				entry.AccessToken = ""
				entry.RefreshToken = ""
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, entry))
				before = f.capture(t)
			case "invalid scope":
				scope = Scope(99)
			case "wrong owner":
				owner.AccountNamespace = "foreign"
			}
			configBefore, err := os.ReadFile(f.path)
			require.NoError(t, err)
			accountPath := filepath.Join(f.root, "accounts", "accounts.json")
			accountBefore, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			publication := f.store.RuntimeSnapshot()
			result, err := f.store.SwitchAuthenticationAccount(ctx, scope, before, owner, "second")
			require.Error(t, err)
			require.False(t, result.AccountRefreshed)
			require.False(t, result.AccountsSaved)
			require.False(t, result.ConfigSaved)
			require.False(t, result.RuntimePublished)
			_, ok := result.RuntimeSnapshot()
			require.False(t, ok)
			configAfter, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, configBefore, configAfter)
			accountAfter, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			require.Equal(t, accountBefore, accountAfter)
			require.True(t, publication.SamePublication(f.store.RuntimeSnapshot()))
		})
	}
}

func TestAuthenticationMutationScopeLockCancellationAndPreparationWithoutLease(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	before := f.capture(t)
	release, err := lock.File(t.Context(), f.store.globalDataPath+".lock")
	require.NoError(t, err)
	defer release()
	prepared := make(chan struct{})
	f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		// This account lock would deadlock if runtime preparation had entered
		// the final account lease prematurely.
		_, err := accounts.Active(ctx, f.owner.AccountNamespace)
		require.NoError(t, err)
		close(prepared)
		return RuntimeGenerationCandidate{Commit: func() { t.Error("blocked mutation committed") }, Abort: func() {}}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := f.store.SwitchAuthenticationAccount(ctx, f.scope, before, f.owner, "second")
		if result.AccountsSaved || result.ConfigSaved || result.RuntimePublished {
			done <- errors.New("canceled scope wait wrote state")
			return
		}
		done <- err
	}()
	select {
	case <-prepared:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime preparation blocked behind scope lock")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("scope lock cancellation blocked")
	}
	require.True(t, before.SameObservation(f.capture(t)))
	_, err = os.Stat(filepath.Join(f.root, "crux.json.lock"))
	require.ErrorIs(t, err, os.ErrNotExist, "read-only source must not receive a scope lock")
}

func TestAuthenticationMutationLogoutRejectsInheritedCredential(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	global := f.store.globalDataPath
	data, err := os.ReadFile(global)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(`{}`)
		err = nil
	}
	require.NoError(t, err)
	data, err = runtimeControlChangeField(data, []string{"providers", "codex", "api_key"}, json.RawMessage(`"synthetic-inherited"`), false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(global, data, 0o600))
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	before := f.capture(t)
	result, err := f.store.LogoutAuthentication(t.Context(), f.scope, before, f.owner)
	require.ErrorContains(t, err, "shadowed")
	require.False(t, result.AccountsSaved)
	require.False(t, result.ConfigSaved)
	require.False(t, result.RuntimePublished)
	require.True(t, before.SameObservation(f.capture(t)))
}

func TestAuthenticationMutationCreatedScopesAndAliases(t *testing.T) {
	for _, topology := range []string{"workspace at cwd", "global at cwd", "parent alias missing", "parent alias existing", "leaf symlink"} {
		t.Run(topology, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			base := map[string]string{}
			for _, pair := range f.store.baseEnvironment.Env() {
				key, value, _ := strings.Cut(pair, "=")
				base[key] = value
			}
			// Relocate configuration before the accepted load. The writable
			// scope can then be genuinely absent from project discovery.
			require.NoError(t, os.Rename(filepath.Join(f.root, "crux.json"), filepath.Join(f.root, "config", "crux.json")))
			if topology == "workspace at cwd" || topology == "global at cwd" || topology == "parent alias missing" {
				path := filepath.Join(f.root, "config", "crux.json")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				for _, kind := range []string{"large", "small"} {
					data, err = runtimeControlChangeField(data, []string{"models", kind}, json.RawMessage(`{"provider":"unrelated","model":"other"}`), false)
					require.NoError(t, err)
				}
				require.NoError(t, os.WriteFile(path, data, 0o600))
			}
			// Explicit owners isolate the transaction from startup migration.
			// Startup alias/discovery receipts have their own regression work.
			sourcePath := filepath.Join(f.root, "config", "crux.json")
			source, err := os.ReadFile(sourcePath)
			require.NoError(t, err)
			for id, owner := range map[string]string{"codex": `{"type":"core","construction":"integrated-codex"}`, "unrelated": `{"type":"custom","construction":"openai-compat"}`} {
				source, err = runtimeControlChangeField(source, []string{"providers", id, "owner"}, json.RawMessage(owner), false)
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(sourcePath, source, 0o600))
			workspace := f.root
			var referent string
			var referentBytes []byte
			f.path = filepath.Join(f.root, "crux.json")
			switch topology {
			case "global at cwd":
				base["CRUX_GLOBAL_DATA"] = f.root
				workspace = filepath.Join(f.root, "new-workspace")
				f.scope = ScopeGlobal
				// A configured provider is pinned into global data at startup.
				// Exercise first authentication so the scope is still absent
				// when switch begins; the other cases retain selected models.
				for _, field := range [][]string{{"providers", "unrelated"}, {"models"}} {
					source, err = runtimeControlChangeField(source, field, nil, true)
					require.NoError(t, err)
				}
				require.NoError(t, os.WriteFile(sourcePath, source, 0o600))
			case "parent alias missing", "parent alias existing":
				linked := filepath.Join(f.root, "linked-data")
				if err := os.Symlink(filepath.Join(f.root, "data"), linked); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				workspace = linked
				f.path = filepath.Join(linked, "crux.json")
				if topology == "parent alias existing" {
					require.NoError(t, os.Rename(filepath.Join(f.root, "workspace-data", "crux.json"), f.path))
				}
			case "leaf symlink":
				referent = filepath.Join(f.root, "workspace-data", "crux.json")
				var err error
				referentBytes, err = os.ReadFile(referent)
				require.NoError(t, err)
				if err := os.Symlink(referent, f.path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			f.store, err = LoadIsolated(f.root, workspace, false, env.NewFromMap(base))
			require.NoError(t, err)
			before := f.capture(t)
			if topology == "workspace at cwd" || topology == "global at cwd" {
				_, err := os.Stat(f.path)
				require.ErrorIs(t, err, os.ErrNotExist, "switch must create the writable scope")
			}

			old := before.runtime.Config()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result, err := f.store.SwitchAuthenticationAccount(ctx, f.scope, before, f.owner, "second")
			require.NoError(t, err)
			require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
			require.True(t, result.After.SameObservation(f.capture(t)))
			require.Equal(t, old.Models, f.store.Config().Models)
			require.Equal(t, old.Agents, f.store.Config().Agents)
			if topology == "workspace at cwd" || topology == "global at cwd" {
				require.Equal(t, len(before.inputs.order)+1, len(result.After.inputs.order))
				count := 0
				for _, path := range result.After.inputs.order {
					if path == f.path {
						count++
					}
				}
				require.Equal(t, 2, count)
			}
			loggedOut, err := f.store.LogoutAuthentication(ctx, f.scope, result.After, f.owner)
			require.NoError(t, err)
			require.True(t, loggedOut.RuntimePublished)
			require.True(t, loggedOut.After.SameObservation(f.capture(t)))
			if referent != "" {
				data, err := os.ReadFile(referent)
				require.NoError(t, err)
				require.Equal(t, referentBytes, data, "rename must not write the old symlink referent")
				info, err := os.Lstat(f.path)
				require.NoError(t, err)
				require.Zero(t, info.Mode()&os.ModeSymlink)
			}
		})
	}
}

func TestAuthenticationMutationScopeLockEntries(t *testing.T) {
	for _, alias := range []string{"parent", "lock symlink", "lock hardlink", "config leaf"} {
		t.Run(alias, func(t *testing.T) {
			root := t.TempDir()
			real := filepath.Join(root, "real")
			require.NoError(t, os.Mkdir(real, 0o700))
			first := filepath.Join(real, "crux.json")
			second := filepath.Join(root, "second.json")
			require.NoError(t, os.WriteFile(first, []byte(`{}`), 0o600))
			require.NoError(t, os.WriteFile(first+".lock", nil, 0o600))
			var err error
			switch alias {
			case "parent":
				linked := filepath.Join(root, "linked")
				err = os.Symlink(real, linked)
				second = filepath.Join(linked, "crux.json")
			case "lock symlink":
				err = os.Symlink(first+".lock", second+".lock")
			case "lock hardlink":
				err = os.Link(first+".lock", second+".lock")
			case "config leaf":
				err = os.Symlink(first, second)
			}
			if err != nil {
				t.Skipf("link unavailable: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			release, err := lockAuthenticationScopes(ctx, authenticationAdmission{globalPath: first, workspacePath: second})
			require.NoError(t, err)
			defer release()
			for _, path := range []string{first, second} {
				unexpected, err := lock.TryFile(path + ".lock")
				if unexpected != nil {
					unexpected()
				}
				require.ErrorIs(t, err, lock.ErrContended)
			}
		})
	}
}

func TestAuthenticationMutationLatePublicationProgress(t *testing.T) {
	for _, action := range []string{"account write", "config write", "caller canceled"} {
		t.Run(action, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			before := f.capture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var prepared *Config
			commits, aborts := 0, 0
			f.store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				prepared = snapshot.Config()
				return RuntimeGenerationCandidate{Commit: func() {
					commits++
					if action == "caller canceled" {
						cancel()
						return
					}
					path := f.path
					if action == "account write" {
						path = filepath.Join(f.root, "accounts", "accounts.json")
					}
					data, err := os.ReadFile(path)
					require.NoError(t, err)
					// A direct external write deliberately ignores the held lease.
					require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
				}, Abort: func() { aborts++ }}, nil
			})
			result, err := f.store.SwitchAuthenticationAccount(ctx, f.scope, before, f.owner, "second")
			require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
			require.Same(t, prepared, f.store.Config())
			require.Equal(t, 1, commits)
			require.Zero(t, aborts)
			_, coherent := result.RuntimeSnapshot()
			if action == "caller canceled" {
				require.NoError(t, err)
				require.True(t, coherent)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.True(t, result.After.SameObservation(f.capture(t)))
			} else {
				require.Error(t, err)
				require.False(t, coherent)
				require.False(t, result.After.inputs.valid)
			}
			active, err := accounts.Active(t.Context(), f.owner.AccountNamespace)
			require.NoError(t, err)
			require.Equal(t, "second", active.ID)
		})
	}
}

func TestAuthenticationMutationRefreshesExactInactivePluginAccount(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "captured-client", r.Form.Get("client_id"))
		require.Equal(t, "synthetic-inactive-refresh", r.Form.Get("refresh_token"))
		_, _ = fmt.Fprint(w, `{"access_token":"synthetic-refreshed-access","refresh_token":"synthetic-refreshed-refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	f := newAuthenticationCandidateFixture(t, "example-responses", false, false, server.URL+"/token")
	t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "accounts"))
	first := accounts.Entry{ID: "active", AccessToken: "synthetic-current", RefreshToken: "synthetic-current-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	second := accounts.Entry{ID: "inactive", AccessToken: "synthetic-expired", RefreshToken: "synthetic-inactive-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, second))
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	old := f.store.Config()
	f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		active, err := accounts.Active(ctx, f.owner.AccountNamespace)
		require.NoError(t, err)
		require.Equal(t, "active", active.ID)
		prospective, captured, err := snapshot.CapturedConstructionAccount(f.owner)
		require.NoError(t, err)
		require.True(t, captured)
		require.Equal(t, "synthetic-refreshed-access", prospective.AccessToken)
		return RuntimeGenerationCandidate{Commit: func() {}, Abort: func() {}}, nil
	})
	result, err := f.store.SwitchAuthenticationAccount(t.Context(), ScopeGlobal, before, f.owner, "inactive")
	require.NoError(t, err)
	require.True(t, result.AccountRefreshed && result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
	require.Equal(t, int32(1), exchanges.Load())
	current, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, result.After.SameObservation(current))
	require.Equal(t, old.Models, f.store.Config().Models)
	require.Equal(t, old.Agents, f.store.Config().Agents)
	provider, ok := f.store.Config().Providers.Get(f.owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, "synthetic-refreshed-access", provider.APIKey)
	count, err := os.ReadFile(f.marker)
	require.NoError(t, err)
	require.Equal(t, "x", string(count), "unconfigured candidate headers evaluate once")
}

func TestAuthenticationMutationShellSourceBoundary(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			path := filepath.Join(f.root, ".cruxrc")
			marker := filepath.Join(f.root, "shell-count")
			var evaluatedBefore []byte
			if alias {
				if err := os.Symlink(f.path, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("printf x >> '%s'\noption notifications bell\n", filepath.ToSlash(marker))), 0o600))
				require.NoError(t, f.store.ReloadFromDisk(t.Context()))
				var err error
				evaluatedBefore, err = os.ReadFile(marker)
				require.NoError(t, err)
			}
			before := f.capture(t)
			result, err := f.store.SwitchAuthenticationAccount(t.Context(), f.scope, before, f.owner, "second")
			if alias {
				require.ErrorContains(t, err, "also a shell configuration source")
				require.False(t, result.AccountRefreshed || result.AccountsSaved || result.ConfigSaved || result.RuntimePublished)
				require.True(t, before.SameObservation(f.capture(t)))
				require.NoFileExists(t, marker)
			} else {
				require.NoError(t, err)
				require.True(t, result.RuntimePublished)
				require.True(t, result.After.SameObservation(f.capture(t)))
				require.Equal(t, "bell", f.store.Config().Options.Notifications)
				data, err := os.ReadFile(marker)
				require.NoError(t, err)
				require.Equal(t, string(evaluatedBefore)+"x", string(data), "preparation evaluates each accepted source once")
			}
		})
	}
}
