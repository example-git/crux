// Command validate_provider_bundles validates provider and provider-preset bundles
// through the same snapshot, compatibility, trust, catalog, and runtime
// projection paths used by Crux.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{
			filepath.FromSlash("docs/provider-plugins/examples"),
			filepath.FromSlash("plugins/provider-presets"),
		}
	}

	bundles, err := discoverBundles(roots)
	if err != nil {
		fatal(err)
	}
	if len(bundles) == 0 {
		fatal(errors.New("no *.plugin bundle directories found"))
	}

	for _, bundle := range bundles {
		pluginType, id, err := validateBundle(context.Background(), bundle)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", filepath.Base(bundle), err))
		}
		fmt.Printf("ok %-15s %s (%s)\n", pluginType, id, filepath.Base(bundle))
	}
	fmt.Printf("validated %d bundle(s)\n", len(bundles))
}

func discoverBundles(roots []string) ([]string, error) {
	seen := make(map[string]struct{})
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve bundle root: %w", err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, fmt.Errorf("open bundle root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("bundle root %q is not a directory", root)
		}
		if strings.HasSuffix(info.Name(), ".plugin") {
			seen[absolute] = struct{}{}
			continue
		}
		err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".plugin") {
				return nil
			}
			seen[path] = struct{}{}
			return filepath.SkipDir
		})
		if err != nil {
			return nil, fmt.Errorf("scan bundle root %q: %w", root, err)
		}
	}

	bundles := make([]string, 0, len(seen))
	for path := range seen {
		bundles = append(bundles, path)
	}
	slices.Sort(bundles)
	return bundles, nil
}

func validateBundle(ctx context.Context, source string) (string, string, error) {
	temporary, err := os.MkdirTemp("", "crux-provider-contract-")
	if err != nil {
		return "", "", fmt.Errorf("create isolated state: %w", err)
	}
	defer os.RemoveAll(temporary)

	manager, err := providerplugin.NewManager(ctx, providerplugin.DefaultPaths(
		filepath.Join(temporary, "data"), filepath.Join(temporary, "cache"),
	))
	if err != nil {
		return "", "", fmt.Errorf("initialize isolated plugin manager: %w", err)
	}
	defer manager.Close()

	snapshot, err := manager.Install(ctx, providerplugin.InstallRequest{
		Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision,
	})
	if err != nil {
		return "", "", err
	}
	if len(snapshot.Plugins) != 1 {
		return "", "", fmt.Errorf("expected one installed bundle, got %d", len(snapshot.Plugins))
	}
	status := snapshot.Plugins[0]
	if status.State != providerplugin.StateRegistered ||
		status.Trust != providerplugin.TrustTrusted ||
		status.Compatibility != providerplugin.CompatibilityCompatible {
		return "", "", fmt.Errorf("bundle is not active: state=%s trust=%s compatibility=%s", status.State, status.Trust, status.Compatibility)
	}
	if len(status.Diagnostics) > 0 {
		return "", "", fmt.Errorf("bundle reported diagnostic %s: %s", status.Diagnostics[0].Code, status.Diagnostics[0].Message)
	}

	switch status.PluginType {
	case manifest.PluginTypeProvider:
		bundles := manager.RegisteredBundles()
		if len(bundles) != 1 {
			return "", "", fmt.Errorf("expected one runtime provider registration, got %d", len(bundles))
		}
		registration, err := providerregistry.FromManifest(bundles[0].Manifest, bundles[0].StaticText)
		if err != nil {
			return "", "", fmt.Errorf("compile runtime provider registration: %w", err)
		}
		if _, err := providerregistry.New(registration); err != nil {
			return "", "", fmt.Errorf("activate runtime provider registration: %w", err)
		}
		providers, err := manager.CatalogProviders()
		if err != nil {
			return "", "", err
		}
		if len(providers) != 1 {
			return "", "", fmt.Errorf("expected one provider catalog projection, got %d", len(providers))
		}
	case manifest.PluginTypeProviderPreset:
		if len(manager.RegisteredPresetBundles()) != 1 {
			return "", "", errors.New("provider preset was not registered")
		}
		if len(manager.CatalogPresets()) != 1 {
			return "", "", errors.New("provider preset was not projected into the catalog")
		}
	default:
		return "", "", fmt.Errorf("unsupported registered plugin type %q", status.PluginType)
	}
	return status.PluginType, status.ID, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "provider bundle validation failed:", err)
	os.Exit(1)
}
