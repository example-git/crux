package codebaseindex

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	cruxdb "github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/githubauth"
)

func CanonicalProjectRoot(ctx context.Context, workingDir string) (string, error) {
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return "", fmt.Errorf("resolve project directory %q: %w", workingDir, err)
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = root
	output, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(output)) != "" {
		root, err = filepath.Abs(strings.TrimSpace(string(output)))
		if err != nil {
			return "", fmt.Errorf("resolve Git worktree root: %w", err)
		}
	}
	return filepath.Clean(root), nil
}

func DefaultDatabasePath(projectRoot string) (string, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root %q: %w", projectRoot, err)
	}
	configHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".ai-cli", "claude-code")
	}

	digest := sha1.Sum([]byte(root))
	projectName := filepath.Base(root)
	if projectName == "." || projectName == string(filepath.Separator) || projectName == "" {
		projectName = "project"
	}
	key := projectName + "-" + hex.EncodeToString(digest[:])[:8]
	return filepath.Join(configHome, "codebase-index", key+".db"), nil
}

func ResolveDatabasePath(ctx context.Context, projectRoot, configuredPath string) (string, error) {
	path, found, err := FindImportDatabasePath(ctx, projectRoot, configuredPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no database contains a compatible codebase index for project %q", projectRoot)
	}
	return path, nil
}

func FindImportDatabasePath(ctx context.Context, projectRoot, configuredPath string) (string, bool, error) {
	path := strings.TrimSpace(configuredPath)
	if path == "" {
		var err error
		path, err = DefaultDatabasePath(projectRoot)
		if err != nil {
			return "", false, err
		}
	} else {
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectRoot, path)
		}
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return "", false, fmt.Errorf("resolve codebase index path %q: %w", configuredPath, err)
		}
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat configured codebase index path %q: %w", path, err)
	}
	if !info.IsDir() {
		found, err := databaseContainsProject(ctx, path, projectRoot)
		if err != nil {
			return "", false, err
		}
		return path, found, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return "", false, fmt.Errorf("read codebase index directory %q: %w", path, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		candidate := filepath.Join(path, entry.Name())
		found, candidateErr := databaseContainsProject(ctx, candidate, projectRoot)
		if candidateErr != nil || !found {
			continue
		}
		return candidate, true, nil
	}
	return "", false, nil
}

func databaseContainsProject(ctx context.Context, path, projectRoot string) (bool, error) {
	database, err := cruxdb.ConnectReadOnly(ctx, path)
	if err != nil {
		return false, fmt.Errorf("open codebase index database %q: %w", path, err)
	}
	var found int
	queryErr := database.QueryRowContext(ctx, `
SELECT 1
FROM chunks
WHERE project_root = ?
LIMIT 1`, projectRoot).Scan(&found)
	closeErr := database.Close()
	if queryErr != nil {
		return false, nil
	}
	if closeErr != nil {
		return false, fmt.Errorf("close codebase index database %q: %w", path, closeErr)
	}
	return true, nil
}

func CodebaseIndexToken(ctx context.Context) (string, error) {
	return githubauth.DefaultLegacyIndexFileSource().Token(ctx, githubauth.CodebaseIndex)
}
