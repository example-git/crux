// Package githubauth provides purpose-scoped GitHub credential sources. A
// credential is never silently reused across codebase indexing and Copilot
// inference purposes.
package githubauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Purpose string

const (
	CodebaseIndex      Purpose = "codebase-index"
	CopilotInference   Purpose = "copilot-inference"
	maxCredentialBytes         = int64(64 << 10)
)

type Source interface {
	Token(context.Context, Purpose) (string, error)
}

// LegacyIndexFileSource reads the current codebase-index credential while its
// interactive replacement is introduced. It refuses use for any other purpose.
type LegacyIndexFileSource struct {
	Path string
	Err  error
}

func DefaultLegacyIndexFileSource() LegacyIndexFileSource {
	dir := os.Getenv("AI_CLI_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return LegacyIndexFileSource{Err: fmt.Errorf("resolve home directory: %w", err)}
		}
		dir = filepath.Join(home, ".ai-cli")
	}
	return LegacyIndexFileSource{Path: filepath.Join(dir, "codebase-index-auth.json")}
}

func (s LegacyIndexFileSource) Token(ctx context.Context, purpose Purpose) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.Err != nil {
		return "", s.Err
	}
	if purpose != CodebaseIndex {
		return "", fmt.Errorf("GitHub credential source does not serve purpose %q", purpose)
	}
	if s.Path == "" {
		return "", fmt.Errorf("resolve home directory for codebase-index credential")
	}
	info, err := os.Lstat(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("inspect codebase-index credential: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maxCredentialBytes {
		return "", fmt.Errorf("codebase-index credential is not a bounded regular file")
	}
	file, err := os.Open(s.Path)
	if err != nil {
		return "", fmt.Errorf("read codebase-index credential: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return "", fmt.Errorf("read codebase-index credential: %w", err)
	}
	if int64(len(data)) > maxCredentialBytes {
		return "", fmt.Errorf("codebase-index credential exceeds %d bytes", maxCredentialBytes)
	}
	var value struct {
		AccessToken string `json:"accessToken"`
		AuthMode    string `json:"authMode"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("parse codebase-index credential: %w", err)
	}
	if value.AuthMode != "vscode" || value.AccessToken == "" {
		return "", fmt.Errorf("codebase-index credential is invalid")
	}
	return value.AccessToken, nil
}
