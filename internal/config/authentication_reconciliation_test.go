package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func reconciliationUnchangedFiles(t *testing.T, paths ...string) func() {
	t.Helper()
	infos := make([]os.FileInfo, len(paths))
	bodies := make([][]byte, len(paths))
	for i, path := range paths {
		var err error
		infos[i], err = os.Stat(path)
		require.NoError(t, err)
		bodies[i], err = os.ReadFile(path)
		require.NoError(t, err)
	}
	return func() {
		t.Helper()
		for i, path := range paths {
			info, err := os.Stat(path)
			require.NoError(t, err)
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(infos[i], info), path)
			require.Equal(t, infos[i].ModTime(), info.ModTime(), path)
			require.Equal(t, bodies[i], body, path)
		}
	}
}

func TestAuthenticationReconciliationCurrentSwitchLogout(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, disabled)
			for _, accountID := range []string{"first", "second", ""} {
				switch accountID {
				case "second":
					_, err := f.store.SwitchAuthenticationAccount(t.Context(), f.scope, f.capture(t), f.owner, accountID)
					require.NoError(t, err)
				case "":
					_, err := f.store.LogoutAuthentication(t.Context(), f.scope, f.capture(t), f.owner)
					require.NoError(t, err)
				}
				capture := f.capture(t)
				unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "crux.json"), filepath.Join(f.root, "accounts", "accounts.json"))
				f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					t.Error("validation attempted runtime preparation")
					return RuntimeGenerationCandidate{}, nil
				})
				for range 2 {
					prepared, err := f.store.PrepareAuthenticationReconciliation(t.Context(), capture, f.owner, AuthenticationReconciliationEffect{Logout: accountID == "", AccountID: accountID})
					require.NoError(t, err)
					exact, ok := prepared.AuthenticationCapture()
					require.True(t, ok)
					require.True(t, capture.SameObservation(exact))
					require.True(t, capture.runtime.SamePublication(f.store.RuntimeSnapshot()))
					_, err = json.Marshal(prepared)
					require.ErrorContains(t, err, "preparations are private")
					for _, format := range []string{"%v", "%+v", "%#v"} {
						text := fmt.Sprintf(format, prepared)
						for _, secret := range []string{f.root, f.owner.AccountNamespace, f.first.AccessToken, f.second.AccessToken} {
							require.NotContains(t, text, secret)
						}
					}
					public, err := exact.Accounts(f.owner)
					require.NoError(t, err)
					if len(public) != 0 {
						public[0].DisplayName = "caller change"
						again, _ := exact.Accounts(f.owner)
						require.NotEqual(t, public, again)
					}
				}
				unchanged()
				f.store.SetRuntimeGenerationPreparer(nil)
			}
		})
	}
}

func TestAuthenticationReconciliationRequiresFullAcceptedBasis(t *testing.T) {
	for _, change := range []string{"target-credential", "other-provider", "unknown-root", "new-priority", "shell", "missing", "unavailable"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			switch change {
			case "target-credential":
				authenticationBasisWriteField(t, f.path, []string{"providers", "codex", "api_key"}, `"unaccepted-target-value"`)
			case "other-provider":
				authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"providers", "unrelated", "extra_headers"}, `{"X-New":"unaccepted"}`)
			case "unknown-root":
				authenticationBasisWriteField(t, f.path, []string{"unrelated_harness"}, `{"value":false}`)
			case "new-priority":
				require.NoError(t, os.WriteFile(filepath.Join(f.root, ".crux.json"), []byte(`{}`), 0o600))
			case "shell":
				marker := filepath.Join(f.root, "shell-not-run")
				require.NoError(t, os.WriteFile(filepath.Join(f.root, ".cruxrc"), []byte(fmt.Sprintf("printf x > '%s'; printf '{}'", marker)), 0o600))
			case "missing":
				require.NoError(t, os.Remove(f.path))
			case "unavailable":
				next := f.store.Config().cloneForWrite()
				next.authenticationBasis = nil
				f.store.setConfig(next)
			}
			capture := f.capture(t) // A fresh Status generation cannot bless the edit.
			prepared, err := f.store.PrepareAuthenticationReconciliation(t.Context(), capture, f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
			require.ErrorIs(t, err, ErrAuthenticationReconciliationReloadRequired)
			_, ok := prepared.AuthenticationCapture()
			require.False(t, ok)
			require.True(t, capture.runtime.SamePublication(f.store.RuntimeSnapshot()))
			require.NoFileExists(t, filepath.Join(f.root, "shell-not-run"))
			for _, format := range []string{"%v", "%#v"} {
				text := fmt.Sprintf(format, err)
				require.NotContains(t, text, f.root)
				require.NotContains(t, text, "unaccepted-target-value")
			}
		})
	}
}

func TestAuthenticationReconciliationCredentialAndOwnerConflicts(t *testing.T) {
	for _, change := range []string{"wrong-account", "active", "token", "expiry", "metadata", "owner", "logout-accounts", "logout-credentials", "invalid-effect"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			owner := f.owner
			effect := AuthenticationReconciliationEffect{AccountID: "first"}
			switch change {
			case "wrong-account":
				effect.AccountID = "second"
			case "active":
				require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, f.second))
			case "token", "expiry":
				entry := f.first
				if change == "token" {
					entry.AccessToken = "synthetic-peer-token"
				} else {
					entry.ExpiresAt += 1000
				}
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, entry))
			case "metadata":
				_, err := f.store.SwitchAuthenticationAccount(t.Context(), f.scope, f.capture(t), owner, "second")
				require.NoError(t, err)
				effect.AccountID = "second"
				entry := f.second
				entry.Raw = json.RawMessage(`{"account_id":"different-execution-account"}`)
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, entry))
			case "owner":
				owner.AccountNamespace += "-foreign"
			case "logout-accounts":
				_, err := f.store.LogoutAuthentication(t.Context(), f.scope, f.capture(t), owner)
				require.NoError(t, err)
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, f.first))
				effect = AuthenticationReconciliationEffect{Logout: true}
			case "logout-credentials":
				effect = AuthenticationReconciliationEffect{Logout: true}
			case "invalid-effect":
				effect.Logout = true
			}
			capture := f.capture(t)
			unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			prepared, err := f.store.PrepareAuthenticationReconciliation(t.Context(), capture, owner, effect)
			require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
			_, ok := prepared.AuthenticationCapture()
			require.False(t, ok)
			require.True(t, capture.runtime.SamePublication(f.store.RuntimeSnapshot()))
			unchanged()
		})
	}
}

func TestAuthenticationReconciliationSelectedDependencyCoherence(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"models", "small"}, `{"provider":"unrelated","model":"other"}`)
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	_, err := f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
	require.NoError(t, err, "an unrelated selected API-key provider does not need an OAuth account")
	authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"providers", "unrelated", "oauth"}, `{"access_token":"synthetic-unrelated"}`)
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
	require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict, "selected OAuth dependencies require their own coherent captured account")

	// An exact image credential owner also participates even when its provider
	// is not selected by either text model.
	next := f.store.Config().cloneForWrite()
	badOwner := f.owner
	badOwner.Construction = providerregistry.ConstructionOpenAICompat
	next.Images = &ImageConfiguration{Providers: map[string]ImageProviderConfiguration{"image": {Credentials: map[string]providerregistry.RegistrationOwner{"credential": badOwner}}}}
	f.store.setConfig(next)
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
	require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
}

func TestAuthenticationReconciliationReusesAcceptedExpressionsWithoutExecution(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	marker := filepath.Join(f.root, "shell-evaluations")
	endpoint := filepath.Join(f.root, "endpoint-not-run")
	require.NoError(t, os.WriteFile(filepath.Join(f.root, ".cruxrc"), []byte(fmt.Sprintf("printf x >> '%s'; printf '{}'", marker)), 0o600))
	encoded, err := json.Marshal(fmt.Sprintf("$(printf x > '%s'; printf https://example.invalid/v1)", endpoint))
	require.NoError(t, err)
	authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"providers", "codex", "base_url"}, string(encoded))
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	count, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.NotEmpty(t, count)
	unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "crux.json"), marker)
	foreign := filepath.Join(t.TempDir(), "not-used")
	t.Setenv("AI_CLI_DIR", foreign)
	t.Setenv("HOME", foreign)
	t.Setenv("USERPROFILE", foreign)
	for range 2 {
		_, err := f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
		require.NoError(t, err)
	}
	unchanged()
	require.NoFileExists(t, endpoint)
	require.NoDirExists(t, foreign)
}

func TestAuthenticationReconciliationStaleCaptureAndCancellation(t *testing.T) {
	for _, change := range []string{"publication", "file", "account", "environment", "canceled-lock"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			capture := f.capture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch change {
			case "publication":
				f.store.setConfig(f.store.Config())
			case "file":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
			case "environment":
				values := map[string]string{}
				for _, entry := range capture.runtime.Environment() {
					key, value, _ := strings.Cut(entry, "=")
					values[key] = value
				}
				values["NEW_CAPTURE_VALUE"] = "changed"
				f.store.effectiveEnvironment = env.NewFromMap(values)
			case "canceled-lock":
				f.store.writeMu.Lock()
				defer f.store.writeMu.Unlock()
				cancel()
			}
			prepared, err := f.store.PrepareAuthenticationReconciliation(ctx, capture, f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
			if change == "canceled-lock" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
			}
			_, ok := prepared.AuthenticationCapture()
			require.False(t, ok)
		})
	}
}

func TestAuthenticationReconciliationCancelWaitingForAccountLease(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	capture := f.capture(t)
	lease, err := capture.accounts.BeginCheck(t.Context())
	require.NoError(t, err)
	defer lease.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.store.PrepareAuthenticationReconciliation(ctx, capture, f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
		done <- err
	}()
	require.Eventually(t, func() bool {
		if f.store.writeMu.TryLock() {
			f.store.writeMu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not cancel while waiting for account lease")
	}
}

func TestAuthenticationReconciliationExplicitReloadAndReplacement(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	before := f.capture(t)
	effect := AuthenticationReconciliationEffect{AccountID: "first"}
	body, err := os.ReadFile(f.path)
	require.NoError(t, err)
	replacement := f.path + ".replacement"
	require.NoError(t, os.WriteFile(replacement, body, 0o600))
	require.NoError(t, os.Rename(replacement, f.path))
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), before, f.owner, effect)
	require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
	current := f.capture(t)
	require.False(t, before.SameObservation(current))
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), current, f.owner, effect)
	require.NoError(t, err, "a reviewed new identity with identical accepted values is coherent")

	authenticationBasisWriteField(t, f.path, []string{"kept_unknown", "reviewed"}, `true`)
	current = f.capture(t)
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), current, f.owner, effect)
	require.ErrorIs(t, err, ErrAuthenticationReconciliationReloadRequired)
	require.NoError(t, f.store.ReloadFromDisk(t.Context()), "reload is a separate explicit action")
	current = f.capture(t)
	unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
	prepared, err := f.store.PrepareAuthenticationReconciliation(t.Context(), current, f.owner, effect)
	require.NoError(t, err)
	exact, valid := prepared.AuthenticationCapture()
	require.True(t, valid)
	require.True(t, exact.SameObservation(current))
	unchanged()

	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
	require.False(t, exact.SameObservation(f.capture(t)))
	retained, valid := prepared.AuthenticationCapture()
	require.True(t, valid)
	require.True(t, exact.SameObservation(retained), "private preparation remains its original point-in-time observation")
}

func TestAuthenticationReconciliationPathlessAndDetached(t *testing.T) {
	t.Run("pathless", func(t *testing.T) {
		store, owner, entry, root := authenticationCaptureTestStore(t)
		capture, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		foreign := t.TempDir()
		for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA"} {
			t.Setenv(key, foreign)
		}
		files := remoteBaselineTree(t, root)
		_, err = store.PrepareAuthenticationReconciliation(t.Context(), capture, owner, AuthenticationReconciliationEffect{AccountID: entry.ID})
		require.ErrorIs(t, err, ErrAuthenticationReconciliationReloadRequired)
		require.Equal(t, files, remoteBaselineTree(t, root))
		children, err := os.ReadDir(foreign)
		require.NoError(t, err)
		require.Empty(t, children)
	})
	t.Run("detached", func(t *testing.T) {
		proposal := remoteRuntimeFixture(t, "minimal.plugin")
		root := t.TempDir()
		store, err := CompileRemoteRuntime(root, filepath.Join(root, "state"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"AI_CLI_DIR": "invalid-relative"}))
		require.NoError(t, err)
		capture := AuthenticationCapture{runtime: store.RuntimeSnapshot()}
		files := remoteBaselineTree(t, root)
		_, err = store.PrepareAuthenticationReconciliation(t.Context(), capture, providerregistry.RegistrationOwner{}, AuthenticationReconciliationEffect{})
		require.ErrorIs(t, err, ErrClientRuntimeManaged)
		require.Equal(t, files, remoteBaselineTree(t, root), "detached authority rejects before path validation or filesystem access")
	})
}

func TestAuthenticationReconciliationCancelWaitingForPublication(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	capture := f.capture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.store.writeMu.Lock()
	defer f.store.writeMu.Unlock()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := f.store.PrepareAuthenticationReconciliation(ctx, capture, f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("validation completed while publication was held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not cancel with publication lock still held")
	}
}

func TestAuthenticationReconciliationDoesNotRefreshExpiredAccount(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	expired := f.first
	expired.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, expired))
	encoded, err := json.Marshal(expired.Token())
	require.NoError(t, err)
	authenticationBasisWriteField(t, f.path, []string{"providers", "codex", "oauth"}, string(encoded))
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	called := false
	f.store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
		called = true
		return nil, errors.New("validation must not refresh")
	}
	unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
	_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: expired.ID})
	require.NoError(t, err, "preparation checks coherence, not token freshness")
	require.False(t, called)
	unchanged()
}

func TestAuthenticationReconciliationUnconfiguredLogout(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	capture := f.capture(t)
	var owner providerregistry.RegistrationOwner
	for _, candidate := range capture.owners {
		if candidate.ProviderID != f.owner.ProviderID && candidate.HasOAuth && candidate.AccountNamespace != "" {
			if _, configured := capture.runtime.config.Providers.Get(candidate.ProviderID); !configured {
				owner = candidate
				break
			}
		}
	}
	require.NotEmpty(t, owner.ProviderID, "fixture must contain an installed unconfigured OAuth registration")
	unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
	_, err := f.store.PrepareAuthenticationReconciliation(t.Context(), capture, owner, AuthenticationReconciliationEffect{Logout: true})
	require.NoError(t, err, "already-empty unconfigured owner needs no synthesized credentials or configuration")
	unchanged()
}

func TestAuthenticationReconciliationCancelWaitingForConfigMutex(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	capture := f.capture(t)
	for _, verifyOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(verifyOnly), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.store.configMu.Lock()
			defer f.store.configMu.Unlock()
			done := make(chan error, 1)
			go func() {
				if verifyOnly {
					f.store.writeMu.RLock()
					defer f.store.writeMu.RUnlock()
					done <- f.store.verifyAuthenticationCollectionLocked(ctx, capture)
					return
				}
				_, err := f.store.PrepareAuthenticationReconciliation(ctx, capture, f.owner, AuthenticationReconciliationEffect{AccountID: "first"})
				done <- err
			}()
			require.Eventually(t, func() bool {
				if f.store.writeMu.TryLock() {
					f.store.writeMu.Unlock()
					return false
				}
				return true
			}, time.Second, time.Millisecond)
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("reconciliation did not cancel with configMu still held")
			}
		})
	}
}

func TestAuthenticationReconciliationCommandCatalogFallbackFailsWithoutExecution(t *testing.T) {
	for _, preset := range []bool{false, true} {
		t.Run(fmt.Sprint(preset), func(t *testing.T) {
			var store *ConfigStore
			var owner providerregistry.RegistrationOwner
			if preset {
				var project string
				store, project, _ = authenticationFallbackFixture(t, "$FALLBACK_KEY", "", false)
				// Put the fixture's credential in the actual writable scope.
				data, err := os.ReadFile(project)
				require.NoError(t, err)
				data, err = runtimeControlChangeField(data, []string{"providers", "authentication-fallback", "api_key"}, nil, true)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(project, data, 0o600))
				require.NoError(t, os.MkdirAll(filepath.Dir(store.workspacePath), 0o700))
				require.NoError(t, os.WriteFile(store.workspacePath, []byte(`{"providers":{"authentication-fallback":{"api_key":"synthetic-scoped-key"}}}`), 0o600))
				require.NoError(t, store.ReloadFromDisk(t.Context()))
				owner, _ = store.RuntimeSnapshot().ProviderOwner("authentication-fallback")
				before, err := store.CaptureAuthentication(t.Context())
				require.NoError(t, err)
				_, err = store.LogoutAuthentication(t.Context(), ScopeWorkspace, before, owner)
				require.NoError(t, err)
			} else {
				f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
				store, owner = f.store, f.owner
				_, err := store.LogoutAuthentication(t.Context(), f.scope, f.capture(t), owner)
				require.NoError(t, err)
			}
			marker := filepath.Join(t.TempDir(), "catalog-command-must-not-run")
			// Preset admission currently rejects command credentials. Inject the
			// retained catalog value here to test the validation boundary for a
			// future source; this is not a claim of installed-command support.
			next := store.Config().cloneForWrite()
			scan := cloneProviderScan(*next.providerScan)
			for i := range scan.Providers {
				if string(scan.Providers[i].ID) == owner.ProviderID {
					scan.Providers[i].APIKey = "${UNSET_RECONCILIATION_VALUE:+$(printf x > '" + marker + "')}"
				}
			}
			next.providerScan = &scan
			store.setConfig(next)
			store.resolver = authenticationFallbackFailResolver{}
			capture, err := store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			files := remoteBaselineTree(t, store.workingDir)
			_, err = store.PrepareAuthenticationReconciliation(t.Context(), capture, owner, AuthenticationReconciliationEffect{Logout: true})
			require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
			require.NotContains(t, err.Error(), marker)
			require.NoFileExists(t, marker)
			require.Equal(t, files, remoteBaselineTree(t, store.workingDir))
		})
	}
}

func TestAuthenticationReconciliationRetainedMetadataSpelling(t *testing.T) {
	for _, change := range []string{"number", "absent-null", "whitespace", "display-name"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			entry := f.second
			entry.Raw = json.RawMessage(`{"value":1}`)
			if change == "absent-null" {
				entry.Raw = nil
			}
			require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, entry))
			_, err := f.store.SwitchAuthenticationAccount(t.Context(), f.scope, f.capture(t), f.owner, entry.ID)
			require.NoError(t, err)
			switch change {
			case "number":
				entry.Raw = json.RawMessage(`{"value":1.0}`)
			case "absent-null":
				entry.Raw = json.RawMessage(`null`)
			case "whitespace":
				entry.Raw = json.RawMessage("{\n  \"value\": 1\n}")
			case "display-name":
				entry.DisplayName = "Updated local label"
			}
			require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, entry))
			unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			_, err = f.store.PrepareAuthenticationReconciliation(t.Context(), f.capture(t), f.owner, AuthenticationReconciliationEffect{AccountID: entry.ID})
			if change == "number" || change == "absent-null" {
				require.ErrorIs(t, err, ErrAuthenticationReconciliationConflict)
			} else {
				require.NoError(t, err)
			}
			unchanged()
		})
	}
}
