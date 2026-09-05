package providerplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type ImageOwner struct {
	Backend  string `json:"backend"`
	PluginID string `json:"plugin_id"`
	Version  string `json:"version"`
	Digest   string `json:"digest"`
}

func (b RegisteredImageBundle) Owner() ImageOwner {
	return ImageOwner{Backend: b.Manifest.Backend, PluginID: b.Manifest.ID, Version: b.Manifest.Version, Digest: b.Digest}
}

func (m *Manager) CaptureImageOwner(backend string) (ImageOwner, error) {
	bundles, err := m.RegisteredImageBundles()
	if err != nil {
		return ImageOwner{}, err
	}
	for _, bundle := range bundles {
		if bundle.Manifest.Backend == backend {
			return bundle.Owner(), nil
		}
	}
	return ImageOwner{}, errors.New("selected image backend is unavailable")
}

func (m *Manager) ImageBundleForOwner(owner ImageOwner) (RegisteredImageBundle, error) {
	bundles, err := m.RegisteredImageBundles()
	if err != nil {
		return RegisteredImageBundle{}, err
	}
	for _, bundle := range bundles {
		if bundle.Owner() == owner {
			return bundle, nil
		}
	}
	return RegisteredImageBundle{}, errors.New("exact image plugin owner is unavailable")
}

func (m *Manager) ValidateImageOwner(ctx context.Context, owner ImageOwner) error {
	if owner.Backend == "" || owner.PluginID == "" || owner.Version == "" || len(owner.Digest) != 64 {
		return errors.New("complete image plugin owner is required")
	}
	if _, err := m.Rescan(ctx, 0); err != nil {
		return errors.New("image plugin ownership could not be revalidated")
	}
	_, err := m.ImageBundleForOwner(owner)
	return err
}

type RegisteredImageBundle struct {
	Manifest manifest.ImageManifest
	Digest   string
}

func (m *Manager) RegisteredImageBundles() ([]RegisteredImageBundle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]RegisteredImageBundle, 0)
	for _, status := range m.state.Plugins {
		if status.State != StateRegistered || status.Trust != TrustTrusted || status.Compatibility != CompatibilityCompatible || status.image == nil {
			continue
		}
		data, err := json.Marshal(status.image)
		if err != nil {
			return nil, errors.New("cannot copy registered image manifest")
		}
		var value manifest.ImageManifest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("cannot decode registered image manifest copy")
		}
		result = append(result, RegisteredImageBundle{Manifest: value, Digest: status.Digest})
	}
	return result, nil
}
