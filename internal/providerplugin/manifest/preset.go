package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/example-git/crux/foundation"
	"golang.org/x/mod/semver"
)

type PresetManifest struct {
	Schema          string                    `json:"$schema,omitempty" jsonschema:"format=uri-reference,description=Optional editor schema hint"`
	PluginType      string                    `json:"plugin_type" jsonschema:"required,enum=provider-preset"`
	ManifestVersion int                       `json:"manifest_version" jsonschema:"required,minimum=1,maximum=1"`
	ID              string                    `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$,maxLength=128"`
	Version         string                    `json:"version" jsonschema:"required,pattern=^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\\+[0-9A-Za-z.-]+)?$,maxLength=128"`
	Name            string                    `json:"name" jsonschema:"required,minLength=1,maxLength=128"`
	Description     string                    `json:"description" jsonschema:"required,minLength=1,maxLength=1024"`
	Publisher       Publisher                 `json:"publisher" jsonschema:"required"`
	Compatibility   Compatibility             `json:"compatibility" jsonschema:"required"`
	Preset          foundation.ProviderPreset `json:"preset" jsonschema:"required"`
}

func DecodePluginType(data []byte) (string, error) {
	if len(data) > MaxManifestBytes {
		return "", fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	var envelope struct {
		PluginType string `json:"plugin_type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("decoding manifest type: %w", err)
	}
	if envelope.PluginType == "" {
		return PluginTypeProvider, nil
	}
	switch envelope.PluginType {
	case PluginTypeProvider, PluginTypeProviderPreset, PluginTypeImageProvider:
		return envelope.PluginType, nil
	default:
		return "", fmt.Errorf("unknown plugin_type %q", envelope.PluginType)
	}
}

func DecodePresetStrict(data []byte) (PresetManifest, error) {
	if len(data) > MaxManifestBytes {
		return PresetManifest{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value PresetManifest
	if err := decoder.Decode(&value); err != nil {
		return PresetManifest{}, fmt.Errorf("decoding provider preset manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return PresetManifest{}, errors.New("manifest contains multiple JSON values")
		}
		return PresetManifest{}, fmt.Errorf("decoding trailing manifest data: %w", err)
	}
	if err := ValidatePreset(value); err != nil {
		return PresetManifest{}, err
	}
	return value, nil
}

func ValidatePreset(value PresetManifest) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if value.PluginType != PluginTypeProviderPreset {
		add("plugin_type must be %q", PluginTypeProviderPreset)
	}
	if value.ManifestVersion != Version {
		add("manifest_version must be %d", Version)
	}
	if !pluginIDPattern.MatchString(value.ID) || len(value.ID) > 128 {
		add("id is invalid")
	}
	if !validSemver(value.Version) {
		add("version must be canonical SemVer without a v prefix")
	}
	if strings.TrimSpace(value.Name) == "" || len(value.Name) > 128 {
		add("name is invalid")
	}
	if strings.TrimSpace(value.Description) == "" || len(value.Description) > 1024 {
		add("description is invalid")
	}
	if !pluginIDPattern.MatchString(value.Publisher.ID) || strings.TrimSpace(value.Publisher.Name) == "" {
		add("publisher is invalid")
	}
	if value.Compatibility.HostAPI.Min < 1 || value.Compatibility.HostAPI.Max < value.Compatibility.HostAPI.Min {
		add("compatibility.host_api must be a nonempty ascending range")
	}
	if bounds := value.Compatibility.HostVersion; bounds != nil {
		if bounds.Min != "" && !validSemver(bounds.Min) {
			add("compatibility.host_version.min is invalid")
		}
		if bounds.Max != "" && !validSemver(bounds.Max) {
			add("compatibility.host_version.max is invalid")
		}
		if bounds.Min != "" && bounds.Max != "" && semver.Compare("v"+bounds.Min, "v"+bounds.Max) > 0 {
			add("compatibility.host_version min exceeds max")
		}
	}
	validateFeatureSets(value.Compatibility, add)
	if err := foundation.ValidateProviderPreset(value.Preset); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
