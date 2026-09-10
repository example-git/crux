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
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Replace only the executable capability of an actually loaded registration.
// All ownership, config layering and account persistence remain production code.
func oauthLoginRegistration(t *testing.T, store *ConfigStore, id string, change func(*providerregistry.Registration)) providerregistry.RegistrationOwner {
	t.Helper()
	registrations := store.providerRegistry.Registrations()
	for i := range registrations {
		if registrations[i].ProviderID == id {
			change(&registrations[i])
			if registrations[i].Identity == nil && registrations[i].Manifest != nil && registrations[i].Manifest.Capabilities.Compatibility != nil {
				registrations[i].Manifest.Capabilities.Compatibility.Delegates = slices.DeleteFunc(registrations[i].Manifest.Capabilities.Compatibility.Delegates, func(value string) bool { return value == "identity" })
			}
		}
	}
	registry, err := providerregistry.New(registrations...)
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	scan := cloneProviderScan(*next.providerScan)
	scan.Registry = registry
	next.bindProviderScan(scan)
	store.providerRegistry = registry
	store.setConfig(next)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(id)
	require.True(t, ok)
	return owner
}

func oauthLoginToken() *oauth.Token {
	return &oauth.Token{AccessToken: "synthetic-new-access", RefreshToken: "synthetic-new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "captured-client"}}
}

func oauthLoginBrowser(t *testing.T, f *authenticationMutationFixture, authorize func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error)) {
	t.Helper()
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.OAuth.Adapter, r.OAuth.Authorize = providerregistry.LoginBrowser, authorize
		r.Identity = func(context.Context, string) (string, string, json.RawMessage) {
			return "third", "Third", json.RawMessage(`{"account_id":"third-owner","number":1.0}`)
		}
	})
}

func oauthLoginAuthorize(t *testing.T, f authenticationMutationFixture) (OAuthLoginPreparation, AuthorizedOAuthPreparation) {
	t.Helper()
	prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
	require.NoError(t, err)
	authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
	require.NoError(t, err)
	return prep, authorized
}

func TestOAuthLoginHTTPSCommitExactOwnerAndScopedState(t *testing.T) {
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-disabled-%t", scope, disabled), func(t *testing.T) {
				f := newAuthenticationMutationFixture(t, scope, disabled)
				marker := filepath.Join(f.root, "must-not-execute")
				token := oauthLoginToken()
				token.AccessToken = fmt.Sprintf("$(touch %s)", marker)
				var requests atomic.Int32
				host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					require.Equal(t, "/token", r.URL.Path)
					_ = json.NewEncoder(w).Encode(token)
				}))
				defer host.Close()
				oauthLoginBrowser(t, &f, func(ctx context.Context, _ providerregistry.OpenURL, _ providerregistry.ReadCode) (*oauth.Token, error) {
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/token", nil)
					if err != nil {
						return nil, err
					}
					response, err := providertransport.ClientWithContextOwnerValidator(ctx, host.Client()).Do(req)
					if err != nil {
						return nil, err
					}
					defer response.Body.Close()
					var result oauth.Token
					err = json.NewDecoder(response.Body).Decode(&result)
					return &result, err
				})
				before := f.capture(t)
				original := before.runtime.Config()
				prep, err := f.store.PrepareOAuthLogin(t.Context(), before, f.owner)
				require.NoError(t, err)
				authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
				require.NoError(t, err)
				require.True(t, before.SameObservation(f.capture(t)), "exchange must not save config or accounts")
				var prepared *Config
				commits := 0
				f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					prepared = snapshot.Config()
					active, err := accounts.Active(ctx, f.owner.AccountNamespace)
					require.NoError(t, err, "preparer must run before acquiring account lease")
					require.Equal(t, "first", active.ID)
					entry, captured, err := snapshot.CapturedConstructionAccount(f.owner)
					require.NoError(t, err)
					require.True(t, captured)
					require.Equal(t, "third", entry.ID)
					require.Equal(t, `{"account_id":"third-owner","number":1.0}`, string(entry.Raw))
					return RuntimeGenerationCandidate{Commit: func() { commits++ }, Abort: func() {}}, nil
				})
				result, err := f.store.CommitOAuthLogin(t.Context(), scope, authorized)
				require.NoError(t, err)
				require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
				require.False(t, result.AccountRefreshed)
				require.Same(t, prepared, f.store.Config())
				require.True(t, result.After.SameObservation(f.capture(t)))
				require.Equal(t, original.Models, prepared.Models)
				require.Equal(t, original.Agents, prepared.Agents)
				provider, _ := prepared.Providers.Get("codex")
				require.Equal(t, token, provider.OAuthToken)
				require.Equal(t, token.AccessToken, provider.APIKey)
				require.Equal(t, disabled, provider.Disable)
				old, _ := original.Providers.Get("codex")
				require.Equal(t, f.first.AccessToken, old.APIKey)
				active, err := accounts.Active(t.Context(), f.owner.AccountNamespace)
				require.NoError(t, err)
				require.Equal(t, "third", active.ID)
				require.Equal(t, token.AccessToken, active.AccessToken)
				require.True(t, authenticationMetadataEqual(authorized.state.entry.Raw, active.Raw))
				disk, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Contains(t, string(disk), "9007199254740993")
				client := gjson.GetBytes(disk, "providers.codex.oauth.client")
				for _, key := range []string{"client_id", "client_secret", "auth_url", "token_url", "auth_style"} {
					require.True(t, client.Get(key).Exists(), key)
				}
				_, err = os.Stat(marker)
				require.ErrorIs(t, err, os.ErrNotExist)
				copy := authorized
				repeated, err := f.store.CommitOAuthLogin(t.Context(), scope, copy)
				require.NoError(t, err)
				require.True(t, repeated.After.SameObservation(result.After))
				require.Equal(t, 1, commits)
				require.Equal(t, int32(1), requests.Load())
				_, err = f.store.CommitOAuthLogin(t.Context(), Scope(-123), copy)
				require.Error(t, err)
				f.store.setConfig(f.store.Config().cloneForWrite())
				historical, err := f.store.CommitOAuthLogin(t.Context(), scope, copy)
				require.NoError(t, err)
				require.True(t, historical.After.SameObservation(result.After))
				require.False(t, historical.After.SameObservation(f.capture(t)), "historical result cannot adopt later state")
			})
		}
	}
}

func TestOAuthLoginCopiedPreparationSharesAttemptAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			secretError := errors.New("synthetic-private-token-error")
			oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
				calls.Add(1)
				close(entered)
				<-release
				if fail {
					return nil, secretError
				}
				return oauthLoginToken(), nil
			})
			prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
			require.NoError(t, err)
			type reply struct {
				result AuthorizedOAuthPreparation
				err    error
			}
			done := make(chan reply, 1)
			go func() {
				result, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
				done <- reply{result, err}
			}()
			<-entered
			copy := prep
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = f.store.AuthorizeOAuthLogin(ctx, copy, nil, nil)
			require.ErrorIs(t, err, context.Canceled)
			close(release)
			first := <-done
			second, err := f.store.AuthorizeOAuthLogin(t.Context(), copy, nil, nil)
			require.Equal(t, int32(1), calls.Load())
			if fail {
				require.Same(t, first.err, err)
				require.ErrorIs(t, err, secretError)
				require.NotContains(t, fmt.Sprintf("%+v", err), secretError.Error())
			} else {
				require.NoError(t, err)
				require.Same(t, first.result.state, second.state)
			}
		})
	}
}

func TestOAuthLoginDeviceCopiesAndPrivateState(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	var requested, polled atomic.Int32
	state := &struct{ Secret string }{"synthetic-device-secret"}
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.Identity = nil
		r.OAuth.Adapter = providerregistry.LoginDeviceCode
		r.OAuth.RequestDeviceCode = func(context.Context) (*providerregistry.DeviceAuthorization, error) {
			requested.Add(1)
			return &providerregistry.DeviceAuthorization{UserCode: "ABC-123", VerificationURL: "https://example.invalid/device", State: state}, nil
		}
		r.OAuth.PollDeviceCode = func(_ context.Context, value *providerregistry.DeviceAuthorization) (*oauth.Token, error) {
			polled.Add(1)
			require.Same(t, state, value.State)
			return oauthLoginToken(), nil
		}
	})
	prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
	require.NoError(t, err)
	device, err := f.store.RequestOAuthDeviceCode(t.Context(), prep)
	require.NoError(t, err)
	copy, err := f.store.RequestOAuthDeviceCode(t.Context(), prep)
	require.NoError(t, err)
	require.Same(t, device.state, copy.state)
	code, url := copy.Interaction()
	require.Equal(t, "ABC-123", code)
	require.Equal(t, "https://example.invalid/device", url)
	authorized, err := f.store.PollOAuthDeviceCode(t.Context(), device)
	require.NoError(t, err)
	again, err := f.store.PollOAuthDeviceCode(t.Context(), copy)
	require.NoError(t, err)
	require.Same(t, authorized.state, again.state)
	require.Equal(t, "default", authorized.state.entry.ID)
	require.Equal(t, int32(1), requested.Load())
	require.Equal(t, int32(1), polled.Load())
	_, err = f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
	require.Error(t, err)
	for _, value := range []any{prep, &prep, device, &device, authorized, &authorized, prep.state, device.state, authorized.state} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			text := fmt.Sprintf(format, value)
			require.NotContains(t, text, state.Secret)
			require.NotContains(t, text, oauthLoginToken().AccessToken)
			require.NotContains(t, text, "captured-client")
		}
		_, err := json.Marshal(value)
		require.Error(t, err)
	}
}
