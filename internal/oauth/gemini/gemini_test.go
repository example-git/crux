package gemini

import (
	"context"
	"strings"
	"testing"
)

func TestOAuthClientCredentialsRequired(t *testing.T) {
	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "")

	opened := false
	_, err := Authorize(context.Background(), func(string) error {
		opened = true
		return nil
	}, func() (string, error) {
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "GEMINI_OAUTH_CLIENT_ID") || !strings.Contains(err.Error(), "GEMINI_OAUTH_CLIENT_SECRET") {
		t.Fatalf("Authorize() error = %v, want missing credential guidance", err)
	}
	if opened {
		t.Fatal("Authorize() opened a browser without configured OAuth credentials")
	}

	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "client-secret")
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		t.Fatalf("oauthClientCredentials() error = %v", err)
	}
	if clientID != "client-id" || clientSecret != "client-secret" {
		t.Fatalf("oauthClientCredentials() = (%q, %q)", clientID, clientSecret)
	}
}

func TestParsePastedCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in        string
		wantCode  string
		wantState string
		wantErr   bool
	}{
		{"4/0AX4Xf", "4/0AX4Xf", "", false},
		{"code=abc&state=xyz", "abc", "xyz", false},
		{"https://antigravity.google/oauth-callback?code=abc&state=xyz", "abc", "xyz", false},
		{"  4/0AX4Xf  ", "4/0AX4Xf", "", false},
		{"", "", "", true},
	}

	for _, tt := range tests {
		code, state, err := parsePastedCode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePastedCode(%q) expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePastedCode(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if code != tt.wantCode || state != tt.wantState {
			t.Errorf("parsePastedCode(%q) = (%q, %q), want (%q, %q)",
				tt.in, code, state, tt.wantCode, tt.wantState)
		}
	}
}
