package accounts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func setup(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	return context.Background()
}

func TestSaveListActive(t *testing.T) {
	ctx := setup(t)
	if err := Save(ctx, ProviderCodex, Entry{ID: "a@x.com", DisplayName: "a@x.com", AccessToken: "tok-a"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, ProviderCodex, Entry{ID: "b@x.com", DisplayName: "b@x.com", AccessToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, ProviderCodex)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, err = %v", list, err)
	}
	active, err := Active(ctx, ProviderCodex)
	if err != nil || active == nil || active.ID != "b@x.com" {
		t.Fatalf("active = %+v, err = %v", active, err)
	}

	// Upsert updates in place without duplicating.
	if err := Save(ctx, ProviderCodex, Entry{ID: "a@x.com", DisplayName: "a@x.com", AccessToken: "tok-a2"}); err != nil {
		t.Fatal(err)
	}
	list, _ = List(ctx, ProviderCodex)
	if len(list) != 2 {
		t.Fatalf("upsert duplicated: %v", list)
	}
	active, _ = Active(ctx, ProviderCodex)
	if active.ID != "a@x.com" || active.AccessToken != "tok-a2" {
		t.Fatalf("active = %+v", active)
	}
}

func TestSetActiveAndRemove(t *testing.T) {
	ctx := setup(t)
	_ = Save(ctx, ProviderGemini, Entry{ID: "one", AccessToken: "t1"})
	_ = Save(ctx, ProviderGemini, Entry{ID: "two", AccessToken: "t2"})

	if err := SetActive(ctx, ProviderGemini, "one"); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, ProviderGemini, "missing"); err == nil {
		t.Fatal("expected error for missing account")
	}

	if err := Remove(ctx, ProviderGemini, "one"); err != nil {
		t.Fatal(err)
	}
	active, _ := Active(ctx, ProviderGemini)
	if active == nil || active.ID != "two" {
		t.Fatalf("active after remove = %+v", active)
	}
	if err := Remove(ctx, ProviderGemini, "two"); err != nil {
		t.Fatal(err)
	}
	active, _ = Active(ctx, ProviderGemini)
	if active != nil {
		t.Fatalf("active should be nil, got %+v", active)
	}
}

func TestExpiredAndRefresh(t *testing.T) {
	ctx := setup(t)
	expired := Entry{
		ID:           "u@x.com",
		AccessToken:  "old",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}
	if !expired.Expired() {
		t.Fatal("entry should be expired")
	}
	_ = Save(ctx, "testprov", expired)

	RegisterRefresher("testprov", func(ctx context.Context, refreshToken string) (*oauth.Token, error) {
		if refreshToken != "refresh-1" {
			return nil, errors.New("wrong refresh token")
		}
		return &oauth.Token{
			AccessToken:  "new",
			RefreshToken: "refresh-2",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	})

	token, err := AccessToken(ctx, "testprov")
	if err != nil {
		t.Fatal(err)
	}
	if token != "new" {
		t.Fatalf("token = %q", token)
	}

	// Refresh must have been persisted with the rotated refresh token.
	active, _ := Active(ctx, "testprov")
	if active.AccessToken != "new" || active.RefreshToken != "refresh-2" {
		t.Fatalf("persisted = %+v", active)
	}
	if active.Expired() {
		t.Fatal("refreshed entry should not be expired")
	}
}

func TestAccessTokenNoAccount(t *testing.T) {
	ctx := setup(t)
	token, err := AccessToken(ctx, "missing-provider")
	if err != nil || token != "" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
}

func TestRemoveProviderClearsAllAccounts(t *testing.T) {
	ctx := setup(t)
	require.NoError(t, Save(ctx, "remove-provider-test", Entry{ID: "one", AccessToken: "token-one"}))
	require.NoError(t, Save(ctx, "remove-provider-test", Entry{ID: "two", AccessToken: "token-two"}))

	require.NoError(t, RemoveProvider(ctx, "remove-provider-test"))
	entries, err := List(ctx, "remove-provider-test")
	require.NoError(t, err)
	require.Empty(t, entries)
	active, err := Active(ctx, "remove-provider-test")
	require.NoError(t, err)
	require.Nil(t, active)
}

func TestStoreKey(t *testing.T) {
	RegisterProvider("synthetic-oauth", "synthetic", 10)
	RegisterProvider("codex", ProviderCodex, 20)
	RegisterProvider("copilot", ProviderCopilot, 30)
	RegisterProvider("gemini-ag", ProviderGemini, 40)
	cases := map[string]string{
		"synthetic-oauth": "synthetic",
		"codex":           ProviderCodex,
		"copilot":         ProviderCopilot,
		"gemini-ag":       ProviderGemini,
		"openai":          "",
	}
	for in, want := range cases {
		if got := StoreKey(in); got != want {
			t.Errorf("StoreKey(%q) = %q, want %q", in, got, want)
		}
	}
}
