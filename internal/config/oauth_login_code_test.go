package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestOAuthLoginCodeChallengeHTTPSRetainedInputAndPublication(t *testing.T) {
	for _, declaration := range []oauth.CallbackRequirement{
		{Mode: "loopback-fixed", Port: 1455, Path: "/auth/callback"},
		{Mode: "loopback-dynamic", Path: "/auth/callback"},
		{Mode: "hosted-paste"},
	} {
		t.Run(declaration.Mode, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			var preparations, exchanges atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				require.Equal(t, "/token", r.URL.Path)
				_ = json.NewEncoder(w).Encode(oauthLoginToken())
			}))
			defer host.Close()
			expiry := time.Now().Add(time.Minute)
			port := uint16(1455)
			if declaration.Mode == "loopback-dynamic" {
				port = 39393
			}
			if declaration.Mode == "hosted-paste" {
				port = 0
			}
			f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
				r.OAuth.Authorize = nil // A split capability has no legacy callback dependency.
				if declaration.Mode == "hosted-paste" {
					r.OAuth.Adapter = providerregistry.LoginHostedPaste
				}
				r.OAuth.Callback = &declaration
				r.Identity = nil
				r.OAuth.PrepareCode = func(ctx context.Context, actual uint16) (*oauth.CodeChallenge, error) {
					preparations.Add(1)
					require.Equal(t, port, actual)
					return oauth.NewCodeChallenge(ctx, host.URL+"/authorize?state=synthetic-private-state", expiry, func(ctx context.Context, input string) (*oauth.Token, error) {
						if input != "synthetic-code" {
							return nil, errors.New("invalid synthetic callback")
						}
						value, ok := oauth.LookupEnvironment(ctx, "AI_CLI_DIR")
						require.True(t, ok)
						require.Equal(t, filepath.Join(f.root, "accounts"), value)
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/token", nil)
						if err != nil {
							return nil, err
						}
						response, err := providertransport.ClientWithContextOwnerValidator(ctx, host.Client()).Do(req)
						if err != nil {
							return nil, err
						}
						defer response.Body.Close()
						var token oauth.Token
						err = json.NewDecoder(response.Body).Decode(&token)
						return &token, err
					})
				}
			})
			before := f.capture(t)
			prep, err := f.store.PrepareOAuthLogin(t.Context(), before, f.owner)
			require.NoError(t, err)
			copiedDescriptor := prep.CallbackRequirement()
			require.Equal(t, declaration, *copiedDescriptor)
			copiedDescriptor.Path = "/wrong"
			require.Equal(t, declaration, *prep.CallbackRequirement())
			_, err = f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
			require.Error(t, err)
			code, err := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, port)
			require.NoError(t, err)
			defer code.Close()
			repeated, err := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, port)
			require.NoError(t, err)
			require.Same(t, code.state, repeated.state)
			require.Equal(t, host.URL+"/authorize?state=synthetic-private-state", code.AuthorizationURL())
			require.Equal(t, expiry, code.ExpiresAt())
			require.Equal(t, int32(1), preparations.Load())
			require.Zero(t, exchanges.Load())
			require.True(t, before.SameObservation(f.capture(t)))
			authorized, err := f.store.ExchangeOAuthCode(t.Context(), code, "synthetic-code")
			require.NoError(t, err)
			again, err := f.store.ExchangeOAuthCode(t.Context(), repeated, "synthetic-code")
			require.NoError(t, err)
			require.Same(t, authorized.state, again.state)
			_, err = f.store.ExchangeOAuthCode(t.Context(), repeated, "different-code")
			require.Error(t, err)
			result, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
			require.NoError(t, err)
			require.True(t, result.RuntimePublished && result.AccountsSaved && result.ConfigSaved)
			require.Equal(t, int32(1), preparations.Load())
			require.Equal(t, int32(1), exchanges.Load())
			for _, value := range []any{code, &code, code.state} {
				_, err := json.Marshal(value)
				require.Error(t, err)
				for _, format := range []string{"%v", "%+v", "%#v"} {
					require.NotContains(t, fmt.Sprintf(format, value), "synthetic-private-state")
				}
			}
		})
	}
}

func TestOAuthLoginCodeChallengeRejectsChangedPortRouteAndInput(t *testing.T) {
	for _, mode := range []string{"bad-port", "changed-port", "monolithic-first", "split-first", "failed-input", "closed", "drift", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			var prepared, exchanged, monolithic atomic.Int32
			sentinel := errors.New("synthetic-private-code-rejection")
			f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
				r.Identity = nil
				r.OAuth.Callback = &oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/auth/callback"}
				r.OAuth.Authorize = func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
					monolithic.Add(1)
					return oauthLoginToken(), nil
				}
				r.OAuth.PrepareCode = func(ctx context.Context, _ uint16) (*oauth.CodeChallenge, error) {
					prepared.Add(1)
					return oauth.NewCodeChallenge(ctx, "https://example.invalid/authorize", time.Time{}, func(context.Context, string) (*oauth.Token, error) { exchanged.Add(1); return nil, sentinel })
				}
			})
			prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
			require.NoError(t, err)
			if mode == "monolithic-first" {
				_, err = f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
				require.NoError(t, err)
				_, err = f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32123)
				require.Error(t, err)
				require.Zero(t, prepared.Load())
				require.Equal(t, int32(1), monolithic.Load())
				return
			}
			if mode == "bad-port" {
				_, err = f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 0)
				require.Error(t, err)
				require.Zero(t, prepared.Load())
				return
			}
			code, err := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32123)
			require.NoError(t, err)
			defer code.Close()
			if mode == "changed-port" {
				_, err = f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32124)
				require.Error(t, err)
				require.Equal(t, int32(1), prepared.Load())
				require.Zero(t, exchanged.Load())
				return
			}
			if mode == "split-first" {
				_, err = f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
				require.Error(t, err)
				require.Zero(t, monolithic.Load())
				require.Zero(t, exchanged.Load())
				return
			}
			if mode == "closed" {
				code.Close()
			}
			if mode == "drift" {
				f.store.setConfig(f.store.Config().cloneForWrite())
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			value, first := f.store.ExchangeOAuthCode(ctx, code, "original-input")
			require.Error(t, first)
			require.Nil(t, value.state)
			if mode == "failed-input" {
				_, again := f.store.ExchangeOAuthCode(t.Context(), code, "original-input")
				require.Same(t, first, again)
				require.ErrorIs(t, first, sentinel)
				require.NotContains(t, first.Error(), sentinel.Error())
				_, err = f.store.ExchangeOAuthCode(t.Context(), code, "new-input")
				require.Error(t, err)
				require.Equal(t, int32(1), exchanged.Load())
			} else {
				require.Zero(t, exchanged.Load())
			}
		})
	}
}

func TestOAuthLoginDeviceExpiryUsesExactInterpreterDeadline(t *testing.T) {
	for _, expiry := range []time.Time{{}, time.Now().Add(37 * time.Second)} {
		f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
		f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
			r.OAuth.Adapter = providerregistry.LoginDeviceCode
			r.OAuth.RequestDeviceCode = func(context.Context) (*providerregistry.DeviceAuthorization, error) {
				return &providerregistry.DeviceAuthorization{UserCode: "code", VerificationURL: "https://example.invalid/device", ExpiresAt: expiry}, nil
			}
			r.OAuth.PollDeviceCode = func(context.Context, *providerregistry.DeviceAuthorization) (*oauth.Token, error) {
				return oauthLoginToken(), nil
			}
		})
		prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
		require.NoError(t, err)
		device, err := f.store.RequestOAuthDeviceCode(t.Context(), prep)
		require.NoError(t, err)
		require.Equal(t, expiry, device.ExpiresAt())
	}
}

func TestOAuthLoginCodePreparationFailureRetainsErrorAndClosesStaleChallenge(t *testing.T) {
	for _, mode := range []string{"factory-error", "factory-error-with-challenge", "capture-drift"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			var calls, exchanges atomic.Int32
			var retained *oauth.CodeChallenge
			sentinel := errors.New("synthetic-private-factory-error")
			f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
				r.OAuth.Callback = &oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"}
				r.OAuth.PrepareCode = func(ctx context.Context, _ uint16) (*oauth.CodeChallenge, error) {
					calls.Add(1)
					if mode == "factory-error" {
						return nil, sentinel
					}
					var err error
					retained, err = oauth.NewCodeChallenge(ctx, "https://example.invalid/authorize", time.Time{}, func(context.Context, string) (*oauth.Token, error) { exchanges.Add(1); return oauthLoginToken(), nil })
					if mode == "factory-error-with-challenge" {
						return retained, sentinel
					}
					f.store.setConfig(f.store.Config().cloneForWrite())
					return retained, err
				}
			})
			prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
			require.NoError(t, err)
			code, first := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32123)
			require.Error(t, first)
			require.Nil(t, code.state)
			_, second := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32123)
			require.Same(t, first, second)
			require.Equal(t, int32(1), calls.Load())
			if mode == "factory-error" || mode == "factory-error-with-challenge" {
				require.ErrorIs(t, first, sentinel)
				require.NotContains(t, first.Error(), sentinel.Error())
			}
			if retained != nil {
				_, err = retained.Exchange(t.Context(), "synthetic-code")
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, exchanges.Load())
			}
		})
	}
}

func TestOAuthLoginCodeConcurrentCopiesJoinOneHTTPSExchange(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	var preparations, exchanges atomic.Int32
	prepareStarted, prepareRelease := make(chan struct{}), make(chan struct{})
	exchangeStarted, exchangeRelease := make(chan struct{}), make(chan struct{})
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		close(exchangeStarted)
		select {
		case <-exchangeRelease:
			_ = json.NewEncoder(w).Encode(oauthLoginToken())
		case <-r.Context().Done():
		}
	}))
	defer host.Close()
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.Identity = nil
		r.OAuth.Callback = &oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"}
		r.OAuth.PrepareCode = func(ctx context.Context, _ uint16) (*oauth.CodeChallenge, error) {
			preparations.Add(1)
			close(prepareStarted)
			select {
			case <-prepareRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return oauth.NewCodeChallenge(ctx, host.URL+"/authorize", time.Time{}, func(ctx context.Context, _ string) (*oauth.Token, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/token", nil)
				if err != nil {
					return nil, err
				}
				response, err := providertransport.ClientWithContextOwnerValidator(ctx, host.Client()).Do(req)
				if err != nil {
					return nil, err
				}
				defer response.Body.Close()
				var token oauth.Token
				err = json.NewDecoder(response.Body).Decode(&token)
				return &token, err
			})
		}
	})
	ownerCtx, cancelOwner := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelOwner()
	prep, err := f.store.PrepareOAuthLogin(ownerCtx, f.capture(t), f.owner)
	require.NoError(t, err)
	type preparedResult struct {
		code OAuthCodeLogin
		err  error
	}
	prepared := make(chan preparedResult, 1)
	go func() {
		code, err := f.store.PrepareOAuthCodeChallenge(ownerCtx, prep, 32123)
		prepared <- preparedResult{code, err}
	}()
	select {
	case <-prepareStarted:
	case <-ownerCtx.Done():
		t.Fatal("code preparation did not start")
	}
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	_, err = f.store.PrepareOAuthCodeChallenge(waitCtx, prep, 32123)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = f.store.AuthorizeOAuthLogin(ownerCtx, prep, nil, nil)
	require.Error(t, err, "an admitted split preparation owns the interaction route")
	close(prepareRelease)
	first := <-prepared
	require.NoError(t, first.err)
	defer first.code.Close()
	repeated, err := f.store.PrepareOAuthCodeChallenge(ownerCtx, prep, 32123)
	require.NoError(t, err)
	require.Same(t, first.code.state, repeated.state)
	type authorizedResult struct {
		value AuthorizedOAuthPreparation
		err   error
	}
	authorized := make(chan authorizedResult, 1)
	go func() {
		value, err := f.store.ExchangeOAuthCode(ownerCtx, first.code, "original-code")
		authorized <- authorizedResult{value, err}
	}()
	select {
	case <-exchangeStarted:
	case <-ownerCtx.Done():
		t.Fatal("HTTPS code exchange did not start")
	}
	waitCtx, cancelWait = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	_, err = f.store.ExchangeOAuthCode(waitCtx, repeated, "original-code")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = f.store.ExchangeOAuthCode(ownerCtx, repeated, "conflicting-code")
	require.Error(t, err)
	const copies = 8
	results := make(chan authorizedResult, copies)
	for range copies {
		go func() {
			value, err := f.store.ExchangeOAuthCode(ownerCtx, repeated, "original-code")
			results <- authorizedResult{value, err}
		}()
	}
	close(exchangeRelease)
	original := <-authorized
	require.NoError(t, original.err)
	for range copies {
		copy := <-results
		require.NoError(t, copy.err)
		require.Same(t, original.value.state, copy.value.state)
	}
	require.Equal(t, int32(1), preparations.Load())
	require.Equal(t, int32(1), exchanges.Load())
	result, err := f.store.CommitOAuthLogin(ownerCtx, f.scope, original.value)
	require.NoError(t, err)
	require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
}

func TestOAuthLoginCanceledCodePreparationDoesNotClaimRouteOrPort(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	var preparations atomic.Int32
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.OAuth.Callback = &oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"}
		r.OAuth.PrepareCode = func(ctx context.Context, port uint16) (*oauth.CodeChallenge, error) {
			preparations.Add(1)
			require.Equal(t, uint16(32124), port)
			return oauth.NewCodeChallenge(ctx, "https://example.invalid/authorize", time.Time{}, func(context.Context, string) (*oauth.Token, error) { return oauthLoginToken(), nil })
		}
	})
	prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.store.AuthorizeOAuthLogin(ctx, prep, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = f.store.PrepareOAuthCodeChallenge(ctx, prep, 32123)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, preparations.Load())
	code, err := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 32124)
	require.NoError(t, err)
	defer code.Close()
	require.Equal(t, int32(1), preparations.Load())
}
