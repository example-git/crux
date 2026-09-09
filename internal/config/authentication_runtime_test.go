package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func runtimeAuthenticationFixture(t *testing.T) (*ConfigStore, AuthenticationCapture, []providerregistry.RegistrationOwner, []accounts.Entry) {
	t.Helper()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	registrations := []providerregistry.Registration{ownerTestRegistration("target", "target.plugin"), ownerTestRegistration("other", "other.plugin"), ownerTestRegistration("absent", "absent.plugin")}
	entries := []accounts.Entry{
		{ID: "old", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh", Raw: json.RawMessage(`{"account_id":"old-account"}`)},
		{ID: "new", AccessToken: "synthetic-new", RefreshToken: "synthetic-new-refresh", Raw: json.RawMessage(`{"account_id":"new-account"}`)},
		{ID: "other", AccessToken: "synthetic-other", Raw: json.RawMessage(`{"account_id":"other-account"}`)},
	}
	require.NoError(t, accounts.Save(t.Context(), registrations[0].AccountNamespace, entries[1]))
	require.NoError(t, accounts.Save(t.Context(), registrations[0].AccountNamespace, entries[0]))
	require.NoError(t, accounts.Save(t.Context(), registrations[1].AccountNamespace, entries[2]))
	store := NewTestStoreWithRegistrations(&Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{"target": forwardedAccountOwnerTestProvider(registrations[0], entries[0].Token()), "other": forwardedAccountOwnerTestProvider(registrations[1], entries[2].Token())}),
		Models:    map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "target", Model: "exact", MaxTokens: 19}},
		Agents:    map[string]Agent{AgentCoder: {ID: AgentCoder, Model: SelectedModelTypeLarge}},
	}, registrations...)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	// Save canonicalizes Raw formatting; compare construction with the exact captured bytes.
	for i, namespace := range []string{registrations[0].AccountNamespace, registrations[0].AccountNamespace, registrations[1].AccountNamespace} {
		for _, captured := range capture.accounts.Entries(namespace) {
			if captured.ID == entries[i].ID {
				entries[i] = captured
				break
			}
		}
	}
	owners := make([]providerregistry.RegistrationOwner, len(registrations))
	for i, r := range registrations {
		owners[i] = r.Owner()
	}
	return store, capture, owners, entries
}

func runtimeAuthenticationCandidate(t *testing.T, store *ConfigStore, capture AuthenticationCapture, owner providerregistry.RegistrationOwner, entry *accounts.Entry) *Config {
	t.Helper()
	next := store.Config().cloneForWrite()
	provider, _ := next.Providers.Get(owner.ProviderID)
	if entry == nil {
		provider.APIKey = ""
		provider.APIKeyTemplate = ""
		provider.OAuthToken = nil
	} else {
		registration, ok := next.ProviderRegistration(owner.ProviderID)
		require.True(t, ok)
		applyOAuthTokenToProvider(&provider, entry.Token(), registration)
	}
	next.Providers.Set(owner.ProviderID, provider)
	require.NoError(t, capture.finalizeRuntimeAuthenticationAccounts(next, owner, entry))
	return next
}

func TestAuthenticationRuntimeAccountsSurvivePublicationAndSnapshotCopies(t *testing.T) {
	store, capture, owners, entries := runtimeAuthenticationFixture(t)
	original := store.Config()
	next := runtimeAuthenticationCandidate(t, store, capture, owners[0], &entries[1])
	require.Nil(t, original.authenticationAccounts)
	require.Equal(t, original.Models, next.Models)
	require.Equal(t, original.Agents, next.Agents)
	store.setConfig(next)
	// Live selection and known absence both change after capture. Construction
	// must still use the admitted complete map, including its unconfigured owner.
	require.NoError(t, accounts.Save(t.Context(), owners[1].AccountNamespace, accounts.Entry{ID: "peer", AccessToken: "peer"}))
	require.NoError(t, accounts.Save(t.Context(), owners[2].AccountNamespace, accounts.Entry{ID: "appeared", AccessToken: "appeared"}))
	t.Setenv("AI_CLI_DIR", filepath.Join(t.TempDir(), "never-read"))
	check := func(snapshot RuntimeSnapshot) {
		require.Same(t, next, snapshot.Config())
		for i, want := range []*accounts.Entry{&entries[1], &entries[2], nil} {
			got, captured, err := snapshot.CapturedConstructionAccount(owners[i])
			require.NoError(t, err)
			require.True(t, captured)
			require.Equal(t, want, got)
			if got != nil && len(got.Raw) > 0 {
				got.Raw[0] = 'x'
			}
		}
	}
	check(store.RuntimeSnapshot())
	require.NoError(t, store.WithRuntimeSnapshot(func(snapshot RuntimeSnapshot) error { check(snapshot); return nil }))
	later := next.cloneForWrite()
	later.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "other", Model: "different", MaxTokens: 23}
	store.setConfig(later)
	require.Same(t, next.authenticationAccounts, later.authenticationAccounts)
	got, captured, err := store.RuntimeSnapshot().CapturedConstructionAccount(owners[1])
	require.NoError(t, err)
	require.True(t, captured)
	require.Equal(t, entries[2], *got)
	require.NoDirExists(t, os.Getenv("AI_CLI_DIR"))
	redacted := later.RedactedForTransport()
	require.Nil(t, redacted.authenticationAccounts)
	data, err := json.Marshal(later)
	require.NoError(t, err)
	require.NotContains(t, string(data), "account_id")
	_, err = json.Marshal(later.authenticationAccounts)
	require.Error(t, err)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		text := fmt.Sprintf(verb, later.authenticationAccounts)
		require.NotContains(t, text, "synthetic-")
		require.NotContains(t, text, "other-account")
	}
}

func TestAuthenticationRuntimeAccountsRejectUnprovenCandidateAndOwner(t *testing.T) {
	store, capture, owners, entries := runtimeAuthenticationFixture(t)
	require.Error(t, capture.finalizeRuntimeAuthenticationAccounts(store.Config(), owners[0], &entries[0]))
	next := store.Config().cloneForWrite()
	require.Error(t, capture.finalizeRuntimeAuthenticationAccounts(next, owners[0], &entries[1]))
	require.Error(t, capture.finalizeRuntimeAuthenticationAccounts(next, owners[0], nil), "logout cannot retain credentials")
	require.Nil(t, next.authenticationAccounts)
	marked := runtimeAuthenticationCandidate(t, store, capture, owners[0], &entries[1])
	store.setConfig(marked)
	wrong := owners[0]
	wrong.ManifestVersion = "2.0.0"
	_, captured, err := store.RuntimeSnapshot().CapturedConstructionAccount(wrong)
	require.True(t, captured)
	require.Error(t, err)
	incomplete := marked.cloneForWrite()
	authority := *marked.authenticationAccounts
	authority.entries = map[providerregistry.RegistrationOwner]*accounts.Entry{}
	incomplete.authenticationAccounts = &authority
	store.setConfig(incomplete)
	_, captured, err = store.RuntimeSnapshot().CapturedConstructionAccount(owners[0])
	require.True(t, captured)
	require.Error(t, err)
	// Ordinary unmarked runtimes preserve the preexisting account lookup path.
	unmarked := store.Config().cloneForWrite()
	unmarked.authenticationAccounts = nil
	store.setConfig(unmarked)
	got, captured, err := store.RuntimeSnapshot().CapturedConstructionAccount(owners[0])
	require.NoError(t, err)
	require.False(t, captured)
	require.Nil(t, got)
}

func TestAuthenticationRuntimeDisabledMaintenanceRemainsScoped(t *testing.T) {
	store, capture, owners, entries := runtimeAuthenticationFixture(t)
	marked := runtimeAuthenticationCandidate(t, store, capture, owners[0], &entries[1])
	for _, owner := range owners[:2] {
		p, _ := marked.Providers.Get(owner.ProviderID)
		p.Disable = true
		marked.Providers.Set(owner.ProviderID, p)
	}
	store.setConfig(marked)
	require.ErrorIs(t, store.RuntimeSnapshot().AuthenticationConstructionDenial(owners[0].ProviderID), ErrAuthenticationProviderDisabled)
	require.NoError(t, store.RuntimeSnapshot().AuthenticationConstructionDenial(owners[1].ProviderID))
	next := marked.cloneForWrite()
	p, _ := next.Providers.Get(owners[0].ProviderID)
	p.Disable = false
	next.Providers.Set(owners[0].ProviderID, p)
	store.setConfig(next)
	require.NoError(t, store.RuntimeSnapshot().AuthenticationConstructionDenial(owners[0].ProviderID))
	require.Equal(t, marked.Models, next.Models)
	require.Equal(t, marked.Agents, next.Agents)
}

func TestAuthenticationRuntimeSelectedRefreshCopyOnWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	store, owner, entry := selectedRefreshFixture(t)
	store.baseEnvironment = snapshotEnvironment()
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	next := runtimeAuthenticationCandidate(t, store, capture, owner, &entry)
	store.setConfig(next)
	old := store.RuntimeSnapshot()
	store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) { return selectedRefreshToken(), nil }
	fresh, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
	require.NoError(t, err)
	previous, _, err := old.CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Equal(t, entry, *previous)
	current, _, err := store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Equal(t, *fresh, *current)
	require.NotSame(t, old.Config().authenticationAccounts, store.Config().authenticationAccounts)
	// A bare token application has no proof of account identity. It must not
	// rewrite old metadata to look as if it belonged to a different credential.
	before := store.Config().authenticationAccounts
	require.NoError(t, store.applyToken(&oauth.Token{AccessToken: "unproven-token"}, owner.ProviderID, owner))
	require.Same(t, before, store.Config().authenticationAccounts)
	got, _, err := store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Equal(t, *fresh, *got)
	p, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, "unproven-token", p.APIKey)
}

func TestAuthenticationRuntimeLogoutAbsenceAndDetachedRejection(t *testing.T) {
	store, capture, owners, _ := runtimeAuthenticationFixture(t)
	next := runtimeAuthenticationCandidate(t, store, capture, owners[0], nil)
	store.setConfig(next)
	got, marked, err := store.RuntimeSnapshot().CapturedConstructionAccount(owners[0])
	require.NoError(t, err)
	require.True(t, marked)
	require.Nil(t, got)
	require.NoError(t, accounts.Save(t.Context(), owners[0].AccountNamespace, accounts.Entry{ID: "replacement", AccessToken: "replacement"}))
	got, marked, err = store.RuntimeSnapshot().CapturedConstructionAccount(owners[0])
	require.NoError(t, err)
	require.True(t, marked)
	require.Nil(t, got)
	detached := capture
	detached.runtime.clientRuntime = &clientRuntimeState{}
	candidate := next.cloneForWrite()
	require.ErrorIs(t, detached.finalizeRuntimeAuthenticationAccounts(candidate, owners[0], nil), ErrClientRuntimeManaged)
	snapshot := store.RuntimeSnapshot()
	snapshot.clientRuntime = &clientRuntimeState{}
	_, marked, err = snapshot.CapturedConstructionAccount(owners[0])
	require.True(t, marked)
	require.ErrorContains(t, err, "client runtime")
}

func TestAuthenticationRuntimeRefreshPartialDoesNotReplaceAuthority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	store, owner, entry := selectedRefreshFixture(t)
	store.baseEnvironment = snapshotEnvironment()
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	next := runtimeAuthenticationCandidate(t, store, capture, owner, &entry)
	store.setConfig(next)
	old := store.RuntimeSnapshot()
	store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
		// The account's rotated credential is durably rescued, but a conflicting
		// provider definition prevents its config/metadata publication.
		store.mutateInMemory(func(cfg *Config) {
			p, _ := cfg.Providers.Get(owner.ProviderID)
			p.ExtraHeaders = map[string]string{"X-Changed": "peer"}
			cfg.Providers.Set(owner.ProviderID, p)
		})
		return selectedRefreshToken(), nil
	}
	fresh, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
	require.ErrorContains(t, err, "account token saved; provider config was not updated")
	require.NotNil(t, fresh)
	require.Same(t, old.Config().authenticationAccounts, store.Config().authenticationAccounts)
	got, _, err := store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Equal(t, entry, *got)
	saved, err := accounts.Active(t.Context(), owner.AccountNamespace)
	require.NoError(t, err)
	require.Equal(t, fresh.AccessToken, saved.AccessToken)
}
