package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

func RefreshTokenFromDisk() (string, bool) {
	return refreshTokenFromFile(tokenFilePath())
}

type importPathKey struct{}

// ContextWithImportEnvironment captures only the platform's token-file path.
// Even an absent root is explicit; it must not fall back to another process
// environment when an owning-client runtime initiated the import.
func ContextWithImportEnvironment(ctx context.Context, getenv func(string) string) context.Context {
	return context.WithValue(ctx, importPathKey{}, tokenFilePathForEnvironment(getenv))
}

func RefreshTokenFromDiskForContext(ctx context.Context) (string, bool) {
	if path, captured := ctx.Value(importPathKey{}).(string); captured {
		return refreshTokenFromFile(path)
	}
	return RefreshTokenFromDisk()
}

func refreshTokenFromFile(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var content map[string]struct {
		User        string `json:"user"`
		OAuthToken  string `json:"oauth_token"`
		GitHubAppID string `json:"githubAppId"`
	}
	if err := json.Unmarshal(data, &content); err != nil {
		return "", false
	}
	if app, ok := content["github.com:Iv1.b507a08c87ecfe98"]; ok {
		return app.OAuthToken, true
	}
	return "", false
}

func tokenFilePath() string {
	return tokenFilePathForEnvironment(os.Getenv)
}

func tokenFilePathForEnvironment(getenv func(string) string) string {
	switch runtime.GOOS {
	case "windows":
		if root := getenv("LOCALAPPDATA"); root != "" {
			return filepath.Join(root, "github-copilot/apps.json")
		}
	default:
		if root := getenv("HOME"); root != "" {
			return filepath.Join(root, ".config/github-copilot/apps.json")
		}
	}
	return ""
}
