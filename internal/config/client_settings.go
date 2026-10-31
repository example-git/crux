package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/fsext"
	"github.com/tidwall/sjson"
)

const MaxClientSettingsBytes = 1 << 20

var (
	ErrClientSettingsVersionConflict = errors.New("configuration was modified since last read; fetch the latest version and retry")
	ErrClientSettingsReadonly        = errors.New("only writes to the native user state file are allowed")
	ErrClientSettingsValidation      = errors.New("invalid client settings")
)

type ClientSettingsEdit struct {
	KeyPath       string          `json:"keyPath"`
	Value         json.RawMessage `json:"value"`
	MergeStrategy string          `json:"mergeStrategy"`
}

type ClientSettingsWriteRequest struct {
	Edits           []ClientSettingsEdit `json:"edits"`
	FilePath        *string              `json:"filePath,omitempty"`
	ExpectedVersion *string              `json:"expectedVersion,omitempty"`
	Model           *OwnedSelectedModel  `json:"model,omitempty"`
}

type ClientSettingsWriteResult struct {
	Version  string `json:"version"`
	FilePath string `json:"filePath"`
}

func (request ClientSettingsWriteRequest) Validate() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %s", ErrClientSettingsValidation, err)
		}
	}()
	if request.Model != nil && (request.Model.Model.Provider == "" || request.Model.Model.Model == "" || request.Model.Model.Provider != request.Model.Owner.ProviderID) {
		return errors.New("client model selection requires its matching native provider owner")
	}
	if request.Edits == nil {
		return errors.New("config/batchWrite requires edits")
	}
	for _, edit := range request.Edits {
		if edit.MergeStrategy != "replace" && edit.MergeStrategy != "upsert" {
			return errors.New("mergeStrategy must be replace or upsert")
		}
		if !json.Valid(edit.Value) {
			return errors.New("config edit requires a JSON value")
		}
		if edit.KeyPath == "remote_control" || edit.KeyPath == "features.remote_control" {
			if strings.TrimSpace(string(edit.Value)) != "null" {
				return fmt.Errorf("config key %q can only be removed; remote control is unavailable", edit.KeyPath)
			}
			continue
		}
		segments := strings.Split(edit.KeyPath, ".")
		if len(segments) < 2 || segments[0] != "desktop" {
			return fmt.Errorf("config key %q has no implemented native write mapping", edit.KeyPath)
		}
		for _, segment := range segments[1:] {
			if segment == "" || strings.ContainsAny(segment, "\\\x00") {
				return errors.New("desktop setting path contains an invalid segment")
			}
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxClientSettingsBytes {
		return errors.New("client settings request exceeds the supported size")
	}
	return nil
}

func desktopSettings(layer map[string]any) (map[string]any, error) {
	clients, exists := layer["client_settings"]
	if !exists || clients == nil {
		return map[string]any{}, nil
	}
	clientMap, ok := clients.(map[string]any)
	if !ok {
		return nil, errors.New("native client settings must be an object")
	}
	settings, exists := clientMap["codex_desktop"]
	if !exists || settings == nil {
		return map[string]any{}, nil
	}
	result, ok := settings.(map[string]any)
	if !ok {
		return nil, errors.New("native Desktop settings must be an object")
	}
	return result, nil
}

func clientSettingsVersion(settings map[string]any) string {
	encoded, _ := json.Marshal(settings)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func mergeClientSetting(current, incoming any) any {
	currentMap, currentObject := current.(map[string]any)
	incomingMap, incomingObject := incoming.(map[string]any)
	if !currentObject || !incomingObject {
		return incoming
	}
	for key, value := range incomingMap {
		currentMap[key] = mergeClientSetting(currentMap[key], value)
	}
	return currentMap
}

func applyClientSetting(settings map[string]any, segments []string, value any, strategy string) error {
	for _, segment := range segments[:len(segments)-1] {
		next, exists := settings[segment]
		if !exists {
			if value == nil {
				return nil
			}
			next = map[string]any{}
			settings[segment] = next
		}
		object, ok := next.(map[string]any)
		if !ok {
			return errors.New("desktop setting path traverses a non-object value")
		}
		settings = object
	}
	key := segments[len(segments)-1]
	if value == nil {
		delete(settings, key)
	} else if strategy == "upsert" {
		settings[key] = mergeClientSetting(settings[key], value)
	} else {
		settings[key] = value
	}
	return nil
}

func (s *ConfigStore) WriteClientSettings(ctx context.Context, request ClientSettingsWriteRequest) (ClientSettingsWriteResult, error) {
	if err := request.Validate(); err != nil {
		return ClientSettingsWriteResult{}, err
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock); err != nil {
		return ClientSettingsWriteResult{}, err
	}
	defer s.writeMu.Unlock()
	if s.RemoteAuthority() != nil {
		return ClientSettingsWriteResult{}, ErrClientRuntimeManaged
	}
	if request.Model != nil {
		if err := s.validateActiveProviderOwnerLocked(s.Config(), request.Model.Owner); err != nil {
			return ClientSettingsWriteResult{}, err
		}
		if !s.Config().IsModelAvailable(request.Model.Model.Provider, request.Model.Model.Model) {
			return ClientSettingsWriteResult{}, fmt.Errorf("native model %q is unavailable from provider %q", request.Model.Model.Model, request.Model.Model.Provider)
		}
	}
	path, err := s.configPath(ScopeGlobal)
	if err != nil {
		return ClientSettingsWriteResult{}, err
	}
	canonical, err := fsext.CanonicalPath(path)
	if err != nil {
		return ClientSettingsWriteResult{}, err
	}
	if request.FilePath != nil {
		if !filepath.IsAbs(*request.FilePath) {
			return ClientSettingsWriteResult{}, ErrClientSettingsReadonly
		}
		requested, resolveErr := fsext.CanonicalPath(*request.FilePath)
		if resolveErr != nil || requested != canonical {
			return ClientSettingsWriteResult{}, ErrClientSettingsReadonly
		}
	}
	result := ClientSettingsWriteResult{FilePath: canonical}
	err = s.atomicWrite(ScopeGlobal, func(current []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var layer map[string]any
		if json.Unmarshal(current, &layer) != nil || layer == nil {
			return nil, errors.New("native user state is not a JSON object")
		}
		settings, err := desktopSettings(layer)
		if err != nil {
			return nil, err
		}
		if request.ExpectedVersion != nil && *request.ExpectedVersion != clientSettingsVersion(settings) {
			return nil, ErrClientSettingsVersionConflict
		}
		updated := string(current)
		writeDesktop := false
		for _, edit := range request.Edits {
			if edit.KeyPath == "remote_control" || edit.KeyPath == "features.remote_control" {
				updated, err = sjson.Delete(updated, edit.KeyPath)
			} else {
				var value any
				if err = json.Unmarshal(edit.Value, &value); err != nil {
					return nil, err
				}
				if err = applyClientSetting(settings, strings.Split(edit.KeyPath, ".")[1:], value, edit.MergeStrategy); err != nil {
					return nil, fmt.Errorf("%w: %s", ErrClientSettingsValidation, err)
				}
				writeDesktop = true
			}
			if err != nil {
				return nil, err
			}
		}
		if writeDesktop {
			encoded, err := json.Marshal(settings)
			if err != nil || len(encoded) > MaxClientSettingsBytes {
				return nil, errors.New("Desktop settings exceed the supported size")
			}
			updated, err = sjson.SetRaw(updated, "client_settings.codex_desktop", string(encoded))
			if err != nil {
				return nil, err
			}
		}
		if request.Model != nil {
			encoded, err := json.Marshal(request.Model.Model)
			if err != nil {
				return nil, err
			}
			updated, err = sjson.SetRaw(updated, "models.large", string(encoded))
			if err != nil {
				return nil, err
			}
		}
		result.Version = clientSettingsVersion(settings)
		return []byte(updated), nil
	})
	if err != nil {
		return ClientSettingsWriteResult{}, err
	}
	if err := s.reloadFromDiskLocked(ctx); err != nil {
		return ClientSettingsWriteResult{}, fmt.Errorf("client settings persisted but could not be published: %w", err)
	}
	if request.Model != nil {
		if err := s.rebindSelectedRemoteAccountsLocked(ctx); err != nil {
			return ClientSettingsWriteResult{}, err
		}
	}
	return result, nil
}
