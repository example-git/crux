package codex

import (
	"context"
	"strings"
	"testing"
)

func TestOAuthClientIDRequired(t *testing.T) {
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "")

	opened := false
	_, err := Authorize(context.Background(), func(string) error {
		opened = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "CODEX_OAUTH_CLIENT_ID") {
		t.Fatalf("Authorize() error = %v, want missing client ID guidance", err)
	}
	if opened {
		t.Fatal("Authorize() opened a browser without a configured OAuth client ID")
	}

	t.Setenv("CODEX_OAUTH_CLIENT_ID", "client-id")
	clientID, err := oauthClientID()
	if err != nil {
		t.Fatalf("oauthClientID() error = %v", err)
	}
	if clientID != "client-id" {
		t.Fatalf("oauthClientID() = %q", clientID)
	}
}
