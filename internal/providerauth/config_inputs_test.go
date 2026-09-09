package providerauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type loadedAuthenticationInputsFixture struct {
	service                   *Service
	store                     *config.ConfigStore
	owner                     providerregistry.RegistrationOwner
	entry                     accounts.Entry
	root, configPath, project string
	accountPath               string
}

func loadedAuthenticationInputs(t *testing.T) loadedAuthenticationInputsFixture {
	t.Helper()
	root := t.TempDir()
	configDir, globalData := filepath.Join(root, "config"), filepath.Join(root, "global-data")
	project, accountDir := filepath.Join(root, "project"), filepath.Join(root, "accounts")
	for _, directory := range []string{configDir, globalData, project} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	t.Setenv("AI_CLI_DIR", accountDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	entry := accounts.Entry{ID: "loaded-account", DisplayName: "Loaded account", AccessToken: "synthetic-loaded-access", RefreshToken: "synthetic-loaded-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	document := map[string]any{
		"providers": map[string]any{"codex": map[string]any{
			"api_key": entry.AccessToken, "oauth": entry.Token(),
			"models": []map[string]any{{"id": "auth-fixture", "name": "Auth fixture", "context_window": 8192, "default_max_tokens": 1024}},
		}},
		"models": map[string]any{
			"large": map[string]any{"provider": "codex", "model": "auth-fixture", "max_tokens": 1024},
			"small": map[string]any{"provider": "codex", "model": "auth-fixture", "max_tokens": 512},
		},
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	path := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountDir,
		"CRUX_GLOBAL_CONFIG": configDir, "CRUX_GLOBAL_DATA": globalData,
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileIntegrated),
	})
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	require.Equal(t, accounts.ProviderCodex, owner.AccountNamespace)
	return loadedAuthenticationInputsFixture{service: New(store, "loaded-workspace"), store: store, owner: owner, entry: entry, root: root, configPath: path, project: project, accountPath: filepath.Join(accountDir, "accounts.json")}
}

type authenticationInputsFileState struct {
	mode   fs.FileMode
	size   int64
	mtime  int64
	digest [sha256.Size]byte
}

func authenticationInputsTree(t *testing.T, root string) map[string]authenticationInputsFileState {
	t.Helper()
	result := map[string]authenticationInputsFileState{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := authenticationInputsFileState{mode: info.Mode(), size: info.Size(), mtime: info.ModTime().UnixNano()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state.digest = sha256.Sum256(data)
		}
		result[path] = state
		return nil
	}))
	return result
}

func TestAuthenticationConfigInputsPublicGenerationsAndAccounts(t *testing.T) {
	f := loadedAuthenticationInputs(t)
	runtimeBefore := f.store.RuntimeSnapshot()
	previous, err := f.service.Status(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, previous.Providers)
	require.Equal(t, "in-sync", statusFor(t, previous, "codex").AccountState)
	firstTree := authenticationInputsTree(t, f.root)
	unchanged, err := f.service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, previous, unchanged)
	require.Equal(t, firstTree, authenticationInputsTree(t, f.root))
	marker := filepath.Join(f.root, "shell-must-not-run")
	changes := []struct {
		name  string
		apply func()
	}{
		{"body", func() {
			data, err := os.ReadFile(f.configPath)
			require.NoError(t, err)
			var document map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &document))
			document["options"] = json.RawMessage(`{"debug":true}`)
			data, err = json.Marshal(document)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(f.configPath, data, 0o600))
		}},
		{"metadata", func() {
			info, err := os.Stat(f.configPath)
			require.NoError(t, err)
			require.NoError(t, os.Chtimes(f.configPath, info.ModTime(), info.ModTime().Add(time.Second)))
		}},
		{"same bytes replacement", func() {
			data, err := os.ReadFile(f.configPath)
			require.NoError(t, err)
			info, err := os.Stat(f.configPath)
			require.NoError(t, err)
			replacement := f.configPath + ".replacement"
			require.NoError(t, os.WriteFile(replacement, data, 0o600))
			require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
			require.NoError(t, os.Rename(replacement, f.configPath))
		}},
		{"new higher priority JSON", func() {
			require.NoError(t, os.WriteFile(filepath.Join(f.project, ".crux.json"), []byte(`{"options":{"debug":false}}`), 0o600))
		}},
		{"new higher priority shell", func() {
			script := []byte("touch '" + marker + "'\nthis source must never run during status\n")
			require.NoError(t, os.WriteFile(filepath.Join(f.project, ".cruxrc"), script, 0o700))
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			oldTarget := Target{WorkspaceID: previous.WorkspaceID, Owner: PublicOwner(f.owner), Generation: previous.Generation}
			change.apply()
			expectedFiles := authenticationInputsTree(t, f.root)
			_, err := f.service.Accounts(t.Context(), oldTarget)
			require.ErrorIs(t, err, ErrStale)
			current, err := f.service.Status(t.Context())
			require.NoError(t, err)
			require.Equal(t, previous.Generation.Epoch, current.Generation.Epoch)
			require.Equal(t, previous.Generation.Sequence+1, current.Generation.Sequence)
			target := Target{WorkspaceID: current.WorkspaceID, Owner: PublicOwner(f.owner), Generation: current.Generation}
			listed, err := f.service.Accounts(t.Context(), target)
			require.NoError(t, err)
			require.Equal(t, target, listed.Target)
			require.Equal(t, "in-sync", listed.Status.AccountState)
			require.Len(t, listed.Accounts, 1)
			require.Equal(t, f.entry.ID, listed.Accounts[0].ID)
			require.True(t, listed.Accounts[0].Active)
			stable, err := f.service.Status(t.Context())
			require.NoError(t, err)
			require.Equal(t, current, stable)
			require.Equal(t, expectedFiles, authenticationInputsTree(t, f.root), "status/accounts must not write config or account state")
			require.True(t, runtimeBefore.SamePublication(f.store.RuntimeSnapshot()), "observing saved-file changes must not load/publish them")
			_, err = os.Stat(marker)
			require.ErrorIs(t, err, os.ErrNotExist)
			previous = current
		})
	}
}

func TestAuthenticationConfigInputsCaptureFailuresStayVisible(t *testing.T) {
	f := loadedAuthenticationInputs(t)
	previous, err := f.service.Status(t.Context())
	require.NoError(t, err)
	for _, kind := range []string{"config input", "account input"} {
		t.Run(kind, func(t *testing.T) {
			path := f.configPath
			if kind == "account input" {
				path = f.accountPath
			}
			backup := path + ".before-error"
			require.NoError(t, os.Rename(path, backup))
			if kind == "config input" {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(`{"private":"synthetic-malformed-account-secret"`), 0o600))
			}
			expectedFiles := authenticationInputsTree(t, f.root)
			failed, err := f.service.Status(t.Context())
			require.Error(t, err)
			require.NotContains(t, err.Error(), f.root)
			require.NotContains(t, err.Error(), "synthetic-malformed-account-secret")
			require.Equal(t, Snapshot{}, failed, "capture failure must not become successful empty provider status")
			target := Target{WorkspaceID: previous.WorkspaceID, Owner: PublicOwner(f.owner), Generation: previous.Generation}
			_, err = f.service.Accounts(t.Context(), target)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrStale, "capture errors must remain visible")
			require.Equal(t, expectedFiles, authenticationInputsTree(t, f.root))
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Rename(backup, path))
			current, err := f.service.Status(t.Context())
			require.NoError(t, err)
			require.NotEmpty(t, current.Providers)
			require.Equal(t, previous.Generation.Sequence+1, current.Generation.Sequence, "failed captures must not consume generations")
			previous = current
		})
	}
}

func TestAuthenticationConfigInputsCancellationBeforeCoherentCapture(t *testing.T) {
	f := loadedAuthenticationInputs(t)
	before, err := f.service.Status(t.Context())
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	guard, cancelGuard := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelGuard()
	go func() {
		done <- accounts.WithSelectedForOwner(guard, f.owner.AccountNamespace, f.entry, func() error { return nil }, func() error {
			close(held)
			select {
			case <-release:
				return nil
			case <-guard.Done():
				return guard.Err()
			}
		})
	}()
	select {
	case <-held:
	case <-guard.Done():
		t.Fatal("account fixture did not acquire its lock")
	}
	files := authenticationInputsTree(t, f.root)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	failed, err := f.service.Status(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, Snapshot{}, failed)
	close(release)
	require.NoError(t, <-done)
	after, err := f.service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "cancellation before account capture must not advance the public generation")
	require.Equal(t, files, authenticationInputsTree(t, f.root))
}
