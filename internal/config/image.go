package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

type ImageConfiguration struct {
	Preferred []providerplugin.ImageOwner           `json:"preferred,omitempty" jsonschema:"description=Ordered exact image owners for automatic selection"`
	Providers map[string]ImageProviderConfiguration `json:"providers,omitempty" jsonschema:"description=Execution-host image plugin configuration keyed by backend"`
}

type ImageProviderConfiguration struct {
	Owner           providerplugin.ImageOwner                     `json:"owner"`
	Configuration   map[string]any                                `json:"configuration,omitempty"`
	Credentials     map[string]providerregistry.RegistrationOwner `json:"credentials,omitempty"`
	BrowserProfiles map[string]string                             `json:"browser_profiles,omitempty"`
}

func cloneImageConfiguration(value *ImageConfiguration) *ImageConfiguration {
	if value == nil {
		return nil
	}
	result := *value
	result.Preferred = slices.Clone(value.Preferred)
	result.Providers = maps.Clone(value.Providers)
	for backend, provider := range result.Providers {
		provider.Configuration = cloneProviderOptions(provider.Configuration)
		provider.Credentials = maps.Clone(provider.Credentials)
		provider.BrowserProfiles = maps.Clone(provider.BrowserProfiles)
		result.Providers[backend] = provider
	}
	return &result
}

func (value *ImageConfiguration) Validate() error {
	if value == nil {
		return nil
	}
	if len(value.Preferred) > 64 || len(value.Providers) > 64 {
		return errors.New("image configuration exceeds provider limit")
	}
	validateOwner := func(owner providerplugin.ImageOwner) error {
		digest, err := hex.DecodeString(owner.Digest)
		if owner.Backend == "" || owner.Backend == "auto" || owner.PluginID == "" || owner.Version == "" || err != nil || len(digest) != 32 || strings.ToLower(owner.Digest) != owner.Digest {
			return errors.New("image configuration requires a complete exact owner")
		}
		return nil
	}
	seen := map[string]bool{}
	for _, owner := range value.Preferred {
		if err := validateOwner(owner); err != nil {
			return err
		}
		if seen[owner.Backend] {
			return errors.New("duplicate preferred image backend")
		}
		seen[owner.Backend] = true
		if provider, ok := value.Providers[owner.Backend]; ok && provider.Owner != owner {
			return errors.New("preferred image owner conflicts with configured owner")
		}
	}
	for backend, provider := range value.Providers {
		if err := validateOwner(provider.Owner); err != nil {
			return err
		}
		if backend != provider.Owner.Backend {
			return errors.New("image configuration backend conflicts with owner")
		}
		data, err := json.Marshal(provider.Configuration)
		if err != nil || len(data) > 1<<20 {
			return errors.New("image configuration must be bounded JSON")
		}
		if len(provider.BrowserProfiles) > 16 {
			return errors.New("image configuration exceeds browser binding limit")
		}
		for id, profile := range provider.BrowserProfiles {
			decoded, err := hex.DecodeString(profile)
			if id == "" || err != nil || len(decoded) != 32 || profile != strings.ToLower(profile) {
				return errors.New("image browser credential requires an explicit profile ID")
			}
		}
		if len(provider.Credentials) > 32 {
			return errors.New("image configuration exceeds credential binding limit")
		}
		for id, owner := range provider.Credentials {
			if id == "" || owner.ProviderID == "" || owner.Construction == "" {
				return errors.New("image credential requires an exact provider owner")
			}
		}
	}
	return nil
}

func (s *ConfigStore) PluginPaths() (providerplugin.Paths, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if s.baseEnvironment == nil {
		return providerplugin.Paths{}, errors.New("execution-host environment is unavailable")
	}
	return providerplugin.DefaultPaths(globalWorkspaceDirFromEnvironment(s.baseEnvironment), globalCacheDirFromEnvironment(s.baseEnvironment)), nil
}

func (s *ConfigStore) HostEnvironment() []string {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if s.baseEnvironment == nil {
		return nil
	}
	return slices.Clone(s.baseEnvironment.Env())
}

func (s *ConfigStore) ImageConfiguration() *ImageConfiguration {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return cloneImageConfiguration(s.Config().Images)
}

func (s *ConfigStore) CompareAndSetImageConfiguration(expected, value *ImageConfiguration) error {
	if err := value.Validate(); err != nil {
		return err
	}
	value = cloneImageConfiguration(value)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !reflect.DeepEqual(s.Config().Images, expected) {
		return errors.New("image configuration changed during setup")
	}
	return s.updateLocked(ScopeGlobal, func(cfg *Config) map[string]any {
		cfg.Images = value
		return map[string]any{"images": value}
	})
}

func (s *ConfigStore) SetImageConfiguration(value *ImageConfiguration) error {
	value = cloneImageConfiguration(value)
	if err := value.Validate(); err != nil {
		return err
	}
	return s.update(ScopeGlobal, func(cfg *Config) map[string]any {
		cfg.Images = value
		return map[string]any{"images": value}
	})
}
