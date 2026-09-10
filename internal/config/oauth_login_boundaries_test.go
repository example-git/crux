package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestOAuthLoginUnconfiguredAndNoAccountNamespace(t *testing.T) {
	for _, noNamespace := range []bool{false, true} {
		t.Run(fmt.Sprint(noNamespace), func(t *testing.T) {
			f := newAuthenticationCandidateFixture(t, "codex", false, false, "")
			identityCalls := 0
			owner := oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
				if noNamespace {
					r.AccountNamespace = ""
				}
				r.OAuth.Authorize = func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
					return oauthLoginToken(), nil
				}
				r.Identity = func(context.Context, string) (string, string, json.RawMessage) { identityCalls++; return "", "", nil }
			})
			before, err := f.store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			prep, err := f.store.PrepareOAuthLogin(t.Context(), before, owner)
			require.NoError(t, err)
			authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
			require.NoError(t, err)
			result, err := f.store.CommitOAuthLogin(t.Context(), ScopeGlobal, authorized)
			require.NoError(t, err)
			require.Equal(t, !noNamespace, result.AccountsSaved)
			require.True(t, result.ConfigSaved && result.RuntimePublished)
			provider, present := f.store.Config().Providers.Get("codex")
			require.True(t, present)
			require.Equal(t, oauthLoginToken().AccessToken, provider.APIKey)
			oldPublic, newPublic := before.runtime.config.cloneForWrite(), f.store.Config().cloneForWrite()
			oldPublic.Providers.Del("codex")
			newPublic.Providers.Del("codex")
			require.Equal(t, mustMarshalConfig(oldPublic), mustMarshalConfig(newPublic))
			require.NotSame(t, before.runtime.config.authenticationBasis, f.store.Config().authenticationBasis)
			require.True(t, f.store.Config().authenticationBasis.valid)
			after, err := f.store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			require.True(t, result.After.SameObservation(after))
			if noNamespace {
				require.Zero(t, identityCalls)
				require.True(t, before.accounts.SameObservation(after.accounts))
			} else {
				require.Equal(t, 1, identityCalls)
				entry, captured, err := result.After.runtime.CapturedConstructionAccount(owner)
				require.NoError(t, err)
				require.True(t, captured)
				require.Equal(t, "default", entry.ID)
				require.Equal(t, "default", entry.DisplayName)
			}
		})
	}
}

func TestOAuthLoginCapturedEnvironmentAndTokenIsolation(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	token := oauthLoginToken()
	raw := json.RawMessage(`{"account_id":"captured","number":1.0}`)
	check := func(ctx context.Context) {
		value, found := oauth.LookupEnvironment(ctx, "AI_CLI_DIR")
		require.True(t, found)
		require.Equal(t, filepath.Join(f.root, "accounts"), value)
		_, found = oauth.LookupEnvironment(ctx, "ABSENT_OAUTH_CONFIG_TEST")
		require.False(t, found)
	}
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.OAuth.Authorize = func(ctx context.Context, _ providerregistry.OpenURL, _ providerregistry.ReadCode) (*oauth.Token, error) {
			check(ctx)
			return token, nil
		}
		r.Identity = func(ctx context.Context, access string) (string, string, json.RawMessage) {
			check(ctx)
			require.Equal(t, token.AccessToken, access)
			return "third", "Third", raw
		}
	})
	prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
	require.NoError(t, err)
	t.Setenv("AI_CLI_DIR", filepath.Join(t.TempDir(), "wrong-live-accounts"))
	t.Setenv("ABSENT_OAUTH_CONFIG_TEST", "must-not-be-used")
	authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
	require.NoError(t, err)
	token.AccessToken, token.Client.ClientID = "caller-mutated", "caller-mutated"
	raw[2] = 'z'
	result, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
	require.NoError(t, err)
	provider, _ := f.store.Config().Providers.Get("codex")
	require.Equal(t, oauthLoginToken().AccessToken, provider.APIKey)
	require.Equal(t, "captured-client", provider.OAuthToken.Client.ClientID)
	entry, _, err := result.After.runtime.CapturedConstructionAccount(f.owner)
	require.NoError(t, err)
	require.Equal(t, `{"account_id":"captured","number":1.0}`, string(entry.Raw))
	require.NoDirExists(t, os.Getenv("AI_CLI_DIR"), "no live account path fallback")
}

func TestOAuthLoginRejectsDriftAtEveryAdmission(t *testing.T) {
	for _, phase := range []string{"prepare", "authorization", "identity", "commit"} {
		for _, change := range []string{"config", "account", "publication"} {
			t.Run(phase+"-"+change, func(t *testing.T) {
				f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
				mutate := func() {
					switch change {
					case "config":
						authenticationBasisWriteField(t, f.path, []string{"unaccepted_peer"}, `"synthetic-peer"`)
					case "account":
						require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
					case "publication":
						f.store.setConfig(f.store.Config().cloneForWrite())
					}
				}
				oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
					if phase == "authorization" {
						mutate()
					}
					return oauthLoginToken(), nil
				})
				if phase == "identity" {
					f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
						r.Identity = func(context.Context, string) (string, string, json.RawMessage) {
							mutate()
							return "third", "Third", nil
						}
					})
				}
				before := f.capture(t)
				if phase == "prepare" {
					mutate()
				}
				prep, err := f.store.PrepareOAuthLogin(t.Context(), before, f.owner)
				if phase == "prepare" {
					require.Error(t, err)
					require.Nil(t, prep.state)
					return
				}
				require.NoError(t, err)
				authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
				if phase != "commit" {
					require.Error(t, err)
					require.Nil(t, authorized.state)
					return
				}
				require.NoError(t, err)
				mutate()
				unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
				result, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
				require.Error(t, err)
				require.False(t, result.AccountsSaved || result.ConfigSaved || result.RuntimePublished)
				unchanged()
			})
		}
	}
}

func TestOAuthLoginFreshCaptureCannotBlessUnacceptedSource(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	var calls atomic.Int32
	oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
		calls.Add(1)
		return oauthLoginToken(), nil
	})
	marker := filepath.Join(f.root, "must-not-run")
	require.NoError(t, os.WriteFile(filepath.Join(f.root, ".cruxrc"), []byte(fmt.Sprintf("touch '%s'; printf '{}'", marker)), 0o600))
	prep, err := f.store.PrepareOAuthLogin(t.Context(), f.capture(t), f.owner)
	require.Error(t, err)
	require.Nil(t, prep.state)
	require.Zero(t, calls.Load())
	require.NoFileExists(t, marker)
}

func TestOAuthLoginScopeShadowAndInvalidScopeDoNotSaveAccount(t *testing.T) {
	for _, scope := range []Scope{ScopeGlobal, Scope(-1)} {
		t.Run(fmt.Sprint(scope), func(t *testing.T) {
			// Workspace credentials outrank the requested global write.
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
				return oauthLoginToken(), nil
			})
			_, authorized := oauthLoginAuthorize(t, f)
			unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			result, err := f.store.CommitOAuthLogin(t.Context(), scope, authorized)
			require.Error(t, err)
			require.False(t, result.AccountsSaved || result.ConfigSaved || result.RuntimePublished)
			unchanged()
		})
	}
}

func TestOAuthLoginHostedUnconfiguredCopilotPreservesHeaderMeaning(t *testing.T) {
	f := newAuthenticationCandidateFixture(t, "copilot", false, false, "")
	var opens, reads atomic.Int32
	owner := oauthLoginRegistration(t, f.store, "copilot", func(r *providerregistry.Registration) {
		r.OAuth.Adapter = providerregistry.LoginHostedPaste
		r.OAuth.Authorize = func(_ context.Context, open providerregistry.OpenURL, read providerregistry.ReadCode) (*oauth.Token, error) {
			if err := open("https://example.invalid/authorize"); err != nil {
				return nil, err
			}
			code, err := read()
			if err != nil {
				return nil, err
			}
			if code != "synthetic-callback" {
				return nil, errors.New("unexpected callback input")
			}
			return oauthLoginToken(), nil
		}
		r.Identity = nil
	})
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	prep, err := f.store.PrepareOAuthLogin(t.Context(), before, owner)
	require.NoError(t, err)
	require.NoFileExists(t, f.marker, "admission does not rerun header commands")
	authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), prep, func(url string) error {
		opens.Add(1)
		require.Equal(t, "https://example.invalid/authorize", url)
		return nil
	}, func() (string, error) { reads.Add(1); return "synthetic-callback", nil })
	require.NoError(t, err)
	result, err := f.store.CommitOAuthLogin(t.Context(), ScopeGlobal, authorized)
	require.NoError(t, err)
	require.True(t, result.RuntimePublished)
	provider, _ := f.store.Config().Providers.Get("copilot")
	require.Equal(t, "accepted-header", provider.ExtraHeaders["X-Captured"])
	require.Equal(t, "command-header", provider.ExtraHeaders["X-Once"])
	require.NoFileExists(t, f.marker, "retained candidate headers must not execute again")
	require.Equal(t, int32(1), opens.Load())
	require.Equal(t, int32(1), reads.Load())
	require.Equal(t, before.runtime.config.Models, f.store.Config().Models)
}

func TestOAuthLoginOwnerAndResultRefusals(t *testing.T) {
	for _, refusal := range []string{"wrong-owner", "wrong-store", "zero", "empty-token", "invalid-raw", "detached"} {
		t.Run(refusal, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
				if refusal == "empty-token" {
					return &oauth.Token{}, nil
				}
				return oauthLoginToken(), nil
			})
			if refusal == "invalid-raw" {
				f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
					r.Identity = func(context.Context, string) (string, string, json.RawMessage) { return "third", "Third", []byte{0xff} }
				})
			}
			before := f.capture(t)
			unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			owner := f.owner
			if refusal == "wrong-owner" {
				owner.AccountNamespace += "-forged"
			}
			if refusal == "detached" {
				f.store.clientRuntime = &clientRuntimeState{}
				_, err := f.store.PrepareOAuthLogin(t.Context(), before, owner)
				require.ErrorIs(t, err, ErrClientRuntimeManaged)
				unchanged()
				return
			}
			prep, err := f.store.PrepareOAuthLogin(t.Context(), before, owner)
			if refusal == "wrong-owner" {
				require.Error(t, err)
				unchanged()
				return
			}
			require.NoError(t, err)
			store := f.store
			if refusal == "wrong-store" {
				store = &ConfigStore{}
			}
			if refusal == "zero" {
				prep = OAuthLoginPreparation{}
			}
			value, err := store.AuthorizeOAuthLogin(t.Context(), prep, nil, nil)
			require.Error(t, err)
			require.Nil(t, value.state)
			_, err = store.CommitOAuthLogin(t.Context(), ScopeGlobal, AuthorizedOAuthPreparation{})
			require.Error(t, err)
			unchanged()
		})
	}
}

func TestOAuthLoginCommitCancellationAndPartialReceipt(t *testing.T) {
	for _, failure := range []string{"write-lock", "config-lock", "preparer", "cancel-preparer", "late-drift"} {
		t.Run(failure, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
				return oauthLoginToken(), nil
			})
			_, authorized := oauthLoginAuthorize(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			var commits, aborts int
			if failure == "preparer" || failure == "cancel-preparer" || failure == "late-drift" {
				f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					if failure == "preparer" {
						return RuntimeGenerationCandidate{}, errors.New("synthetic runtime refusal")
					}
					if failure == "cancel-preparer" {
						cancel()
					}
					return RuntimeGenerationCandidate{Abort: func() { aborts++ }, Commit: func() {
						commits++
						data, err := os.ReadFile(f.path)
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
					}}, nil
				})
			}
			if failure == "write-lock" {
				f.store.writeMu.Lock()
			}
			if failure == "config-lock" {
				f.store.configMu.Lock()
			}
			type reply struct {
				result AuthenticationMutationResult
				err    error
			}
			done := make(chan reply, 1)
			go func() { result, err := f.store.CommitOAuthLogin(ctx, f.scope, authorized); done <- reply{result, err} }()
			if failure == "write-lock" || failure == "config-lock" {
				select {
				case <-done:
					t.Fatal("commit passed a held configuration lock")
				case <-time.After(20 * time.Millisecond):
				}
				cancel()
			}
			var first reply
			select {
			case first = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled OAuth commit blocked")
			}
			if failure == "write-lock" {
				f.store.writeMu.Unlock()
			}
			if failure == "config-lock" {
				f.store.configMu.Unlock()
			}
			require.Error(t, first.err)
			if failure == "late-drift" {
				require.True(t, first.result.AccountsSaved && first.result.ConfigSaved && first.result.RuntimePublished)
				_, valid := first.result.RuntimeSnapshot()
				require.False(t, valid)
				require.Equal(t, 1, commits)
				require.Zero(t, aborts)
			} else {
				require.False(t, first.result.AccountsSaved || first.result.ConfigSaved || first.result.RuntimePublished)
				unchanged()
			}
			if failure == "preparer" || failure == "cancel-preparer" || failure == "late-drift" {
				second, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
				require.Same(t, first.err, err)
				require.Equal(t, first.result.RuntimePublished, second.RuntimePublished)
				if failure == "cancel-preparer" {
					require.Equal(t, 1, aborts)
				}
			}
		})
	}
}

func TestOAuthLoginCancellationAfterDurableWritesFinishesExactReceipt(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	oauthLoginBrowser(t, &f, func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
		return oauthLoginToken(), nil
	})
	_, authorized := oauthLoginAuthorize(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var prepared *Config
	f.store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		prepared = snapshot.Config()
		return RuntimeGenerationCandidate{Commit: cancel, Abort: func() { t.Error("durably committed candidate aborted") }}, nil
	})
	result, err := f.store.CommitOAuthLogin(ctx, f.scope, authorized)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
	require.Same(t, prepared, f.store.Config())
	require.True(t, result.After.SameObservation(f.capture(t)))
	unchanged := reconciliationUnchangedFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
	again, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
	require.NoError(t, err)
	require.True(t, again.After.SameObservation(result.After))
	unchanged()
}

func TestOAuthLoginDeviceFailedAttemptCannotPollAgain(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	var requested, polled atomic.Int32
	sentinel := errors.New("synthetic-private-exchange-body")
	f.owner = oauthLoginRegistration(t, f.store, "codex", func(r *providerregistry.Registration) {
		r.OAuth.Adapter = providerregistry.LoginDeviceCode
		r.OAuth.RequestDeviceCode = func(context.Context) (*providerregistry.DeviceAuthorization, error) {
			requested.Add(1)
			return &providerregistry.DeviceAuthorization{UserCode: "ABC-123", VerificationURL: "https://example.invalid/device"}, nil
		}
		r.OAuth.PollDeviceCode = func(context.Context, *providerregistry.DeviceAuthorization) (*oauth.Token, error) {
			polled.Add(1)
			return nil, sentinel
		}
	})
	before := f.capture(t)
	prep, err := f.store.PrepareOAuthLogin(t.Context(), before, f.owner)
	require.NoError(t, err)
	device, err := f.store.RequestOAuthDeviceCode(t.Context(), prep)
	require.NoError(t, err)
	_, first := f.store.PollOAuthDeviceCode(t.Context(), device)
	_, second := f.store.PollOAuthDeviceCode(t.Context(), device)
	require.Same(t, first, second)
	require.ErrorIs(t, first, sentinel)
	require.NotContains(t, first.Error(), sentinel.Error())
	require.Equal(t, int32(1), requested.Load())
	require.Equal(t, int32(1), polled.Load())
	require.True(t, before.SameObservation(f.capture(t)))
}
