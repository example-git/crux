package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/githubauth"
)

// RemoteCodebaseIndex is private runtime input. Paths, when present, were
// explicitly chosen for this remote workspace, never copied from local tools.
type RemoteCodebaseIndex struct {
	Settings          ToolCodebaseSearch `json:"settings"`
	AccessToken       string             `json:"access_token,omitempty"`
	CredentialInvalid bool               `json:"credential_invalid,omitempty"`
}

func RemoteCodebaseIndexScope(connection, path string) string {
	data, _ := json.Marshal([]string{connection, path})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *ConfigStore) BindRemoteCodebaseIndexScope(connection, path string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.remoteCodebaseIndexScope = RemoteCodebaseIndexScope(connection, path)
}

func cloneCodebaseSettings(settings ToolCodebaseSearch) ToolCodebaseSearch {
	settings.Enabled = clonePointer(settings.Enabled)
	settings.IncludePaths = slices.Clone(settings.IncludePaths)
	settings.ExcludePaths = slices.Clone(settings.ExcludePaths)
	return settings
}

func (s RuntimeSnapshot) collectCodebaseIndex(ctx context.Context) *RemoteCodebaseIndex {
	settings := cloneCodebaseSettings(s.config.Tools.CodebaseSearch)
	// Local source databases and stores have no meaning on the execution host.
	settings.DatabasePath, settings.StoreDirectory, settings.ANNDirectory = "", "", ""
	if saved, ok := s.config.RemoteCodebaseIndexes[s.remoteCodebaseIndexScope]; ok {
		settings = cloneCodebaseSettings(saved)
	}
	if !settings.IsEnabled() && settings.Enabled == nil && len(settings.IncludePaths) == 0 && len(settings.ExcludePaths) == 0 {
		return nil
	}
	value := &RemoteCodebaseIndex{Settings: settings}
	if settings.IsEnabled() {
		var err error
		value.AccessToken, err = s.CodebaseIndexToken(ctx)
		value.CredentialInvalid = err != nil
	}
	return value
}

// CodebaseIndexToken never consults the execution host for a client-owned
// snapshot, including when the owning client has no indexing credential.
func (s RuntimeSnapshot) CodebaseIndexToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.RuntimeRevocation(); err != nil {
		return "", err
	}
	if s.clientRuntime != nil {
		value := s.clientRuntime.proposal.CodebaseIndex
		if value == nil || !value.Settings.IsEnabled() {
			return "", nil
		}
		if value.CredentialInvalid {
			return "", errors.New("the owning client's codebase-index GitHub credential is invalid; repair it on that client and retry")
		}
		return value.AccessToken, nil
	}
	if s.environment == nil {
		return githubauth.DefaultLegacyIndexFileSource().Token(ctx, githubauth.CodebaseIndex)
	}
	dir := s.environment.Get("AI_CLI_DIR")
	if dir == "" {
		home := s.environment.Get("HOME")
		if home == "" {
			home = s.environment.Get("USERPROFILE")
		}
		if home == "" {
			return "", errors.New("client home directory is unavailable for codebase indexing")
		}
		dir = filepath.Join(home, ".ai-cli")
	}
	return (githubauth.LegacyIndexFileSource{Path: filepath.Join(dir, "codebase-index-auth.json")}).Token(ctx, githubauth.CodebaseIndex)
}

func (value *RemoteCodebaseIndex) validate() error {
	if value == nil {
		return nil
	}
	if len(value.AccessToken) > 64<<10 || strings.ContainsAny(value.AccessToken, "\r\n\x00") {
		return errors.New("invalid client codebase-index credential")
	}
	if value.CredentialInvalid && value.AccessToken != "" || !value.Settings.IsEnabled() && value.AccessToken != "" {
		return errors.New("unavailable client codebase index cannot carry a credential")
	}
	for _, path := range []string{value.Settings.DatabasePath, value.Settings.GetStoreDirectory()} {
		if strings.ContainsRune(path, 0) {
			return errors.New("invalid remote codebase-index path")
		}
	}
	return nil
}

// ResolveRemoteCodebaseIndexSettings interprets explicit paths on the execution
// host and keeps the default store inside this principal's workspace data.
func ResolveRemoteCodebaseIndexSettings(settings ToolCodebaseSearch, workingDir, dataDir string) ToolCodebaseSearch {
	settings = cloneCodebaseSettings(settings)
	if settings.GetStoreDirectory() == "" {
		settings.StoreDirectory = filepath.Join(dataDir, "codebase-index")
	}
	for _, path := range []*string{&settings.DatabasePath, &settings.StoreDirectory, &settings.ANNDirectory} {
		*path = strings.TrimSpace(*path)
		if *path != "" && !filepath.IsAbs(*path) {
			*path = filepath.Join(workingDir, *path)
		}
	}
	return settings
}
