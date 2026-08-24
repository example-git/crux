package useragent

import (
	"strings"
	"testing"
)

func TestGeminiUserAgentShape(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "9.9.9")
	ua := Gemini()
	if !strings.HasPrefix(ua, "antigravity/cli/9.9.9 ") {
		t.Fatalf("unexpected UA: %q", ua)
	}
}

func TestVersionValidation(t *testing.T) {
	for _, bad := range []string{"", "notaversion", "<html>", "1", "v1.2.3\n"} {
		if fullVersionRe.MatchString(bad) {
			t.Fatalf("%q should not validate", bad)
		}
	}
	for _, good := range []string{"1.0.9", "2.1.236", "1.0.4-beta.1"} {
		if !fullVersionRe.MatchString(good) {
			t.Fatalf("%q should validate", good)
		}
	}
}

func TestLiveDetection(t *testing.T) {
	t.Logf("gemini UA: %s", Gemini())
}

func TestCopilotIdentityVSCode(t *testing.T) {
	t.Setenv("COPILOT_ADVERTISE_MODE", "vscode")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "0.45.2026041705")
	id := Copilot()
	if id.Mode != CopilotModeVSCode {
		t.Fatalf("mode = %s", id.Mode)
	}
	if id.UserAgent != "GitHubCopilotChat/0.45.2026041705" {
		t.Fatalf("ua = %q", id.UserAgent)
	}
	if id.IntegrationID != "vscode-chat" || id.EditorVersion == "" || id.EditorPluginVersion != "copilot-chat/0.45.2026041705" {
		t.Fatalf("identity = %+v", id)
	}
	if CopilotGitHubUserAgent() != id.UserAgent {
		t.Fatal("github UA should match extension UA in vscode mode")
	}
}

func TestCopilotIdentityCLI(t *testing.T) {
	t.Setenv("COPILOT_ADVERTISE_MODE", "cli")
	t.Setenv("COPILOT_CLI_VERSION", "1.0.32")
	id := Copilot()
	if id.Mode != CopilotModeCLI {
		t.Fatalf("mode = %s", id.Mode)
	}
	if !strings.HasPrefix(id.UserAgent, "copilot/1.0.32 (") || !strings.Contains(id.UserAgent, ") term/") {
		t.Fatalf("ua = %q", id.UserAgent)
	}
	if id.IntegrationID != "copilot-developer-cli" || id.EditorVersion != "" || id.EditorPluginVersion != "" {
		t.Fatalf("identity = %+v", id)
	}
	if CopilotGitHubUserAgent() != "copilot-developer-cli" {
		t.Fatal("github UA should be integration id in cli mode")
	}
}

func TestCodexUserAgentShape(t *testing.T) {
	t.Setenv("CODEX_VERSION", "7.7.7")
	ua := Codex()
	if !strings.HasPrefix(ua, "codex_cli_rs/7.7.7 (") || !strings.Contains(ua, "; ") {
		t.Fatalf("unexpected UA: %q", ua)
	}
}
