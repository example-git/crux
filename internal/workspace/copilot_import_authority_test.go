package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clientCopilotImportTransport func(*http.Request) (*http.Response, error)

func (f clientCopilotImportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientCopilotImportAuthorityWaitIsCancelable(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.w.authority.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := f.w.ImportCopilot(ctx, f.owner)
		done <- err
	}()
	select {
	case err := <-done:
		f.w.authority.mu.Unlock()
		t.Fatalf("import bypassed held authority lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		f.w.authority.mu.Unlock()
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		f.w.authority.mu.Unlock()
		<-done
		t.Fatal("canceled import waited for the owning-client authority lock")
	}
	require.Zero(t, f.puts.Load())
}

func TestClientCopilotImportRejectsChangedAuthority(t *testing.T) {
	for _, changed := range []string{"workspace", "mode", "principal"} {
		t.Run(changed, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			f.w.authority.mu.Lock()
			done := make(chan error, 1)
			go func() {
				_, err := f.w.ImportCopilot(t.Context(), f.owner)
				done <- err
			}()
			select {
			case err := <-done:
				f.w.authority.mu.Unlock()
				t.Fatalf("import bypassed held authority lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			f.w.mu.Lock()
			switch changed {
			case "workspace":
				f.w.ws.ID += "-changed"
			case "mode":
				copy := *f.w.ws.Authority
				copy.Mode = "server"
				f.w.ws.Authority = &copy
			case "principal":
				copy := *f.w.ws.Authority
				copy.Principal = strings.Repeat("f", 64)
				f.w.ws.Authority = &copy
			}
			f.w.mu.Unlock()
			f.w.authority.mu.Unlock()
			require.Error(t, <-done)
			require.Zero(t, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
		})
	}
}

func TestClientCopilotLogoutImportThroughTLS(t *testing.T) {
	for _, mode := range []string{"acknowledged logout", "pending logout requires recovery", "principal changes during exchange"} {
		t.Run(mode, func(t *testing.T) {
			recovery := mode == "pending logout requires recovery"
			f := newClientAuthenticationFixture(t, false)
			t.Setenv("COPILOT_ADVERTISE_MODE", "")
			for _, path := range []string{filepath.Join(f.root, ".config/github-copilot/apps.json"), filepath.Join(f.root, "github-copilot/apps.json")} {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(`{"github.com:Iv1.b507a08c87ecfe98":{"oauth_token":"synthetic-import-source"}}`), 0o600))
			}
			var exchanges atomic.Int32
			exchange := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				assert.Equal(t, "/copilot_internal/v2/token", r.URL.Path)
				assert.Equal(t, "Bearer synthetic-import-source", r.Header.Get("Authorization"))
				if mode == "principal changes during exchange" {
					f.w.mu.Lock()
					changed := *f.w.ws.Authority
					changed.Principal = strings.Repeat("f", 64)
					f.w.ws.Authority = &changed
					f.w.mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-imported-client", "expires_at": time.Now().Add(time.Hour).Unix()})
			}))
			t.Cleanup(exchange.Close)
			endpoint, err := url.Parse(exchange.URL)
			require.NoError(t, err)
			provider, ok := f.store.Config().Providers.Get(f.owner.ProviderID)
			require.True(t, ok)
			inference, err := url.Parse(provider.BaseURL)
			require.NoError(t, err)
			previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
			var mu sync.Mutex
			var importedHeaders []http.Header
			routed := clientCopilotImportTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.github.com" && r.URL.Path == "/copilot_internal/v2/token" {
					clone := r.Clone(r.Context())
					u := *r.URL
					u.Scheme, u.Host = endpoint.Scheme, endpoint.Host
					clone.URL = &u
					return exchange.Client().Transport.RoundTrip(clone)
				}
				if r.URL.Host != inference.Host {
					return nil, fmt.Errorf("unexpected host outside isolated Copilot fixture")
				}
				if r.Header.Get("Authorization") == "Bearer synthetic-imported-client" {
					mu.Lock()
					importedHeaders = append(importedHeaders, r.Header.Clone())
					mu.Unlock()
				}
				return previousTransport.RoundTrip(r)
			})
			http.DefaultTransport, http.DefaultClient = routed, &http.Client{Transport: routed}
			t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = previousTransport, previousClient })
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			coordinator := receiver.App.CurrentAgentCoordinator()
			retained := coordinator.Model()
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("use the imported account")}}
			_, err = retained.Model.Generate(t.Context(), call)
			require.NoError(t, err)
			models := f.store.RuntimeSnapshot().AgentModelState()
			request := providerauth.LogoutRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t)}
			if recovery {
				f.putMode.Store(1)
			}
			outcome, err := f.w.LogoutProvider(t.Context(), request)
			if recovery {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, outcome.ValidateLogout(request))
			require.True(t, outcome.Progress.AccountsSaved && outcome.Progress.ConfigSaved && outcome.Progress.RuntimePublished)
			loggedOut := f.store.RuntimeSnapshot()
			entry, captured, err := loggedOut.CapturedConstructionAccount(f.owner)
			require.NoError(t, err)
			require.True(t, captured)
			require.Nil(t, entry)
			// Both import files and account authority must use the environment
			// captured before this process-global replacement.
			other := t.TempDir()
			t.Setenv("HOME", other)
			t.Setenv("USERPROFILE", other)
			t.Setenv("LOCALAPPDATA", other)
			t.Setenv("AI_CLI_DIR", filepath.Join(other, "accounts"))
			if recovery {
				paths := []string{f.path, f.accountsPath}
				infos, bodies := clientAuthenticationFiles(t, paths...)
				found, err := f.w.ImportCopilot(t.Context(), f.owner)
				require.ErrorContains(t, err, "recover the saved authentication operation")
				require.False(t, found)
				require.Zero(t, exchanges.Load())
				requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
				require.Same(t, loggedOut.Config(), f.store.Config())
				require.EqualValues(t, 1, f.puts.Load())
				f.putMode.Store(0)
				_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
				require.NoError(t, err)
			}
			_, err = coordinator.Model().Model.Generate(t.Context(), call)
			require.Error(t, err, "acknowledged logout denies receiver construction")
			found, err := f.w.ImportCopilot(t.Context(), f.owner)
			if mode == "principal changes during exchange" {
				require.ErrorContains(t, err, "Copilot credentials saved; client authority changed before publication")
				require.False(t, found)
				require.EqualValues(t, 1, exchanges.Load())
				require.EqualValues(t, 1, f.puts.Load(), "a changed principal must not receive an import publication")
				entry, captured, err := f.store.RuntimeSnapshot().CapturedConstructionAccount(f.owner)
				require.NoError(t, err)
				require.True(t, captured)
				require.NotNil(t, entry, "the reported local save remains observable")
				require.Equal(t, "synthetic-imported-client", entry.AccessToken)
				require.True(t, f.w.authority.removed[f.owner])
				return
			}
			require.NoError(t, err)
			require.True(t, found)
			require.EqualValues(t, 1, exchanges.Load())
			entry, captured, err = f.store.RuntimeSnapshot().CapturedConstructionAccount(f.owner)
			require.NoError(t, err)
			require.True(t, captured)
			require.NotNil(t, entry)
			require.Equal(t, "default", entry.ID)
			require.Equal(t, "synthetic-imported-client", entry.AccessToken)
			require.Equal(t, "synthetic-import-source", entry.RefreshToken)
			observation, err := accounts.CaptureStateAt(t.Context(), f.accountsPath, []string{f.owner.AccountNamespace})
			require.NoError(t, err)
			require.Equal(t, entry.ID, observation.ActiveID(f.owner.AccountNamespace))
			require.Equal(t, []accounts.Entry{*entry}, observation.Entries(f.owner.AccountNamespace))
			var accepted *accounts.Entry
			for _, binding := range f.w.authority.accepted.Credentials {
				if binding.Owner == f.owner {
					require.False(t, binding.Unavailable)
					accepted = binding.Account
				}
			}
			require.Equal(t, entry, accepted, "the exact saved account must reach the acknowledged private proposal")
			require.False(t, f.w.authority.removed[f.owner])
			require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
			_, err = coordinator.Model().Model.Generate(t.Context(), call)
			require.NoError(t, err)
			require.Equal(t, []string{"Bearer " + f.first.AccessToken, "Bearer synthetic-imported-client"}, f.observed())
			mu.Lock()
			headers := append([]http.Header(nil), importedHeaders...)
			mu.Unlock()
			require.Len(t, headers, 1)
			require.Equal(t, "retained", headers[0].Get("X-Keep"))
			require.Equal(t, "user", headers[0].Get("X-Initiator"))
			for key, value := range copilot.Headers() {
				require.Equal(t, value, headers[0].Get(key), key)
			}
			entry, captured, err = loggedOut.CapturedConstructionAccount(f.owner)
			require.NoError(t, err)
			require.True(t, captured)
			require.Nil(t, entry, "the prior logout snapshot remains immutable")
			_, err = f.w.ProviderAuthentication(t.Context())
			require.NoError(t, err, "fresh status must agree with the accepted imported account")
			_, err = retained.Model.Generate(t.Context(), call)
			require.Error(t, err, "an old revoked model cannot revive after import: observed credentials %v", f.observed())
			require.NoDirExists(t, filepath.Join(other, "accounts"))
		})
	}
}
