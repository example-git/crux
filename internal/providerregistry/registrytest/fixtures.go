// Package registrytest contains explicit manifest registrations for tests.
package registrytest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerplugin/manifest/manifesttest"
	"github.com/example-git/crux/internal/providerregistry"
)

func Registrations() []providerregistry.Registration {
	var result []providerregistry.Registration
	for _, registration := range providerregistry.Integrated() {
		if registration.Construction == providerregistry.ConstructionCopilot {
			result = append(result, registration)
			continue
		}
		value, err := providerregistry.FromManifest(manifesttest.Delegated(registration.ProviderID), manifesttest.StaticText())
		if err != nil {
			panic(err)
		}
		if err := providerregistry.ValidateActivation(value); err != nil {
			panic(err)
		}
		result = append(result, value)
	}
	return result
}

func Provider(providerID string) providerregistry.Registration {
	registration, err := providerregistry.FromManifest(manifesttest.Delegated(providerID), manifesttest.StaticText())
	if err != nil {
		panic(err)
	}
	return registration
}

func Install(ctx context.Context, dataRoot, cacheRoot string, value manifest.Manifest) error {
	root, err := os.MkdirTemp("", "crux-delegated-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "fixture.plugin")
	if err := os.Mkdir(source, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600); err != nil {
		return err
	}
	for _, file := range Bundle(value).Files {
		if file.Path == "manifest.json" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, file.Path)), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(source, file.Path), file.Data, 0o600); err != nil {
			return err
		}
	}
	manager, err := providerplugin.NewManager(ctx, providerplugin.DefaultPaths(dataRoot, cacheRoot))
	if err != nil {
		return err
	}
	defer manager.Close()
	_, err = manager.Install(ctx, providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	return err
}

// BundleFor binds a test's literal inference URL and catalog into its bundle.
func BundleFor(providerID, baseURL string, models []catalog.Model) (providerregistry.Registration, providerplugin.TransportBundle, error) {
	value := manifesttest.Delegated(providerID)
	for i := range value.Capabilities.Endpoints {
		e := &value.Capabilities.Endpoints[i]
		if e.ID != value.Capabilities.Operations[0].Endpoint {
			continue
		}
		if baseURL != "" {
			u, err := url.Parse(baseURL)
			if err != nil {
				return providerregistry.Registration{}, providerplugin.TransportBundle{}, err
			}
			e.BaseURL, e.AllowedSchemes, e.AllowedHosts = baseURL, []string{u.Scheme}, []string{u.Hostname()}
		}
	}
	if len(models) > 0 {
		value.Models = nil
		for _, m := range models {
			model := manifest.Model{ID: m.ID, Name: m.Name, ContextWindow: max(32000, m.ContextWindow), DefaultMaxTokens: max(1024, m.DefaultMaxTokens), Modalities: manifest.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}}}
			if model.Name == "" {
				model.Name = model.ID
			}
			if m.CanReason {
				model.Reasoning = &manifest.Reasoning{Levels: slices.Clone(m.ReasoningLevels), Default: m.DefaultReasoningEffort}
			}
			value.Models = append(value.Models, model)
		}
		value.Provider.DefaultLargeModel, value.Provider.DefaultSmallModel = value.Models[0].ID, value.Models[0].ID
	}
	registration, err := providerregistry.FromManifest(value, manifesttest.StaticText())
	return registration, Bundle(value), err
}

func Bundle(value manifest.Manifest) providerplugin.TransportBundle {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	files := []providerplugin.TransportFile{{Path: "manifest.json", Data: data}}
	if value.Capabilities.Instructions != nil {
		for _, path := range value.Capabilities.Instructions.Profiles {
			if text, ok := manifesttest.StaticText()[path]; ok {
				files = append(files, providerplugin.TransportFile{Path: path, Data: []byte(text)})
			}
		}
	}
	ordered := slices.Clone(files)
	slices.SortFunc(ordered, func(a, b providerplugin.TransportFile) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, file := range ordered {
		fileHash := sha256.Sum256(file.Data)
		fmt.Fprintf(h, "file\x00%s\x00600\x00%d\x00%s\x00", file.Path, len(file.Data), hex.EncodeToString(fileHash[:]))
	}
	return providerplugin.TransportBundle{Digest: hex.EncodeToString(h.Sum(nil)), Files: files}
}

func Models(providerID string) []catalog.Model {
	declaration := manifesttest.Delegated(providerID)
	result := make([]catalog.Model, len(declaration.Models))
	for i, model := range declaration.Models {
		result[i] = catalog.Model{ID: model.ID, Name: model.Name, ContextWindow: model.ContextWindow, DefaultMaxTokens: model.DefaultMaxTokens, SupportsImages: true, CanReason: model.Reasoning != nil, ReasoningLevels: model.Reasoning.Levels, DefaultReasoningEffort: model.Reasoning.Default}
	}
	return result
}
