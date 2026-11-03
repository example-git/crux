package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/env"
	"github.com/qjebbs/go-jsons"
)

type ConfigReadOrigin struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version"`
}

type ConfigReadView struct {
	Values         map[string]string           `json:"values"`
	Origins        map[string]ConfigReadOrigin `json:"origins"`
	Desktop        map[string]any              `json:"desktop,omitempty"`
	DesktopOrigins map[string]ConfigReadOrigin `json:"desktop_origins,omitempty"`
}

var configReadPaths = []string{"models.large.model", "models.large.provider", "memories.consolidation_model"}

func (s *ConfigStore) ReadConfigView(ctx context.Context, scope Scope) (ConfigReadView, error) {
	if scope != ScopeGlobal && scope != ScopeWorkspace {
		return ConfigReadView{}, errors.New("invalid configuration read scope")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return ConfigReadView{}, err
	}
	defer s.writeMu.RUnlock()
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.clientRuntime != nil {
		return ConfigReadView{}, ErrClientRuntimeManaged
	}
	return readConfigView(ctx, s.config, s.overrides, scope)
}

func readConfigView(ctx context.Context, cfg *Config, overrides RuntimeOverrides, scope Scope) (ConfigReadView, error) {
	if scope != ScopeGlobal && scope != ScopeWorkspace {
		return ConfigReadView{}, errors.New("invalid configuration read scope")
	}
	if cfg == nil || cfg.authenticationBasis == nil || !cfg.authenticationBasis.valid {
		return ConfigReadView{}, errors.New("accepted configuration provenance is unavailable")
	}
	basis := cfg.authenticationBasis
	view := ConfigReadView{Values: map[string]string{}, Origins: map[string]ConfigReadOrigin{}}
	merged := []byte("{}")
	for _, path := range basis.order {
		kind, found := basis.roles[path]
		if !found {
			return ConfigReadView{}, errors.New("accepted configuration source role is unavailable")
		}
		if scope == ScopeGlobal && kind != "system" && kind != "user" {
			continue
		}
		source, exists := basis.sources[path]
		if !exists {
			return ConfigReadView{}, errors.New("accepted configuration source is unavailable")
		}
		if !source.exists || len(source.evaluated) == 0 {
			continue
		}
		var layer map[string]any
		if err := json.Unmarshal(source.evaluated, &layer); err != nil {
			return ConfigReadView{}, errors.New("accepted configuration source is invalid")
		}
		var err error
		merged, err = jsons.Merge([][]byte{merged, source.evaluated})
		if err != nil {
			return ConfigReadView{}, errors.New("accepted configuration merge failed")
		}
		var effective map[string]any
		if err := json.Unmarshal(merged, &effective); err != nil {
			return ConfigReadView{}, errors.New("accepted configuration merge is invalid")
		}
		layerDesktop, err := desktopSettings(layer)
		if err != nil {
			return ConfigReadView{}, err
		}
		effectiveDesktop, err := desktopSettings(effective)
		if err != nil {
			return ConfigReadView{}, err
		}
		if len(effectiveDesktop) > 0 {
			view.Desktop = effectiveDesktop
			if view.DesktopOrigins == nil {
				view.DesktopOrigins = map[string]ConfigReadOrigin{}
			}
			for key := range view.DesktopOrigins {
				if _, exists := effectiveDesktop[key]; !exists {
					delete(view.DesktopOrigins, key)
				}
			}
			for key := range layerDesktop {
				view.DesktopOrigins[key] = ConfigReadOrigin{Kind: kind, Path: path, Version: clientSettingsVersion(layerDesktop)}
			}
		} else {
			view.Desktop = nil
			view.DesktopOrigins = nil
		}
		version := configReadVersion(configReadValues(layer))
		for _, field := range configReadPaths {
			value, found := configReadValue(effective, field)
			if !found {
				delete(view.Values, field)
				delete(view.Origins, field)
				continue
			}
			view.Values[field] = value
			if _, supplied := configReadValue(layer, field); supplied {
				view.Origins[field] = ConfigReadOrigin{Kind: kind, Path: path, Version: version}
			}
		}
	}
	if selected, pinned := overrides.Models[SelectedModelTypeLarge]; pinned {
		values := map[string]string{"models.large.model": selected.Model, "models.large.provider": selected.Provider}
		version := configReadVersion(values)
		for field, value := range values {
			view.Values[field] = value
			view.Origins[field] = ConfigReadOrigin{Kind: "runtime", Version: version}
		}
	}
	return view, ctx.Err()
}

func (b *authenticationLoadBasis) assignReadRoles(environment env.Env, workspacePath string) {
	if b == nil {
		return
	}
	globals := map[string]bool{}
	for _, path := range lookupConfigsFromEnvironment("", environment, true) {
		globals[path] = true
	}
	b.roles = make(map[string]string, len(b.order))
	for _, path := range b.order {
		kind := "project"
		switch {
		case globals[path]:
			kind = "user"
			if path == filepath.Clean(systemConfigPath) {
				kind = "system"
			}
		case workspacePath != "" && path == filepath.Clean(workspacePath):
			kind = "workspace"
		}
		b.roles[path] = kind
	}
}

func configReadValues(layer map[string]any) map[string]string {
	values := map[string]string{}
	for _, path := range configReadPaths {
		if value, exists := configReadValue(layer, path); exists {
			values[path] = value
		}
	}
	return values
}

func configReadValue(layer map[string]any, path string) (string, bool) {
	var value any = layer
	for _, key := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		value = object[key]
	}
	text, ok := value.(string)
	return text, ok
}

func configReadVersion(values map[string]string) string {
	data, _ := json.Marshal(values)
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (view ConfigReadView) Validate() error {
	allowed := map[string]bool{}
	for _, path := range configReadPaths {
		allowed[path] = true
	}
	for field := range view.Values {
		if !allowed[field] {
			return errors.New("configuration read contains an unsupported field")
		}
	}
	for field, origin := range view.Origins {
		if _, exists := view.Values[field]; !exists {
			return errors.New("configuration origin has no value")
		}
		if err := validateConfigReadOrigin(origin); err != nil {
			return err
		}
	}
	for field := range view.Desktop {
		if field == "" {
			return errors.New("Desktop configuration contains an empty setting name")
		}
		if _, exists := view.DesktopOrigins[field]; !exists {
			return errors.New("Desktop configuration has no origin")
		}
	}
	for field, origin := range view.DesktopOrigins {
		if _, exists := view.Desktop[field]; !exists {
			return errors.New("Desktop configuration origin has no value")
		}
		if origin.Kind == "runtime" {
			return errors.New("Desktop configuration must have a native file origin")
		}
		if err := validateConfigReadOrigin(origin); err != nil {
			return err
		}
	}
	if view.Values == nil || view.Origins == nil {
		return errors.New("configuration read is incomplete")
	}
	return nil
}

func validateConfigReadOrigin(origin ConfigReadOrigin) error {
	switch origin.Kind {
	case "system", "user", "project", "workspace":
		if !filepath.IsAbs(origin.Path) {
			return errors.New("configuration origin path must be absolute")
		}
	case "runtime":
		if origin.Path != "" {
			return errors.New("runtime configuration origin cannot name a file")
		}
	default:
		return errors.New("configuration origin kind is unsupported")
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(origin.Version, "sha256:"))
	if err != nil || len(digest) != sha256.Size || !strings.HasPrefix(origin.Version, "sha256:") {
		return errors.New("configuration origin version is invalid")
	}
	return nil
}
