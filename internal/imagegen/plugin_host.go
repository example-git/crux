package imagegen

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func NewHostPluginRuntime(ctx context.Context, store *config.ConfigStore, bindings PluginCredentialBindings) (*PluginRuntime, error) {
	if store == nil {
		return nil, errors.New("image plugin configuration is unavailable")
	}
	paths, err := store.PluginPaths()
	if err != nil {
		return nil, err
	}
	manager, err := providerplugin.NewManager(ctx, paths)
	if err != nil {
		return nil, err
	}
	runtime := &PluginRuntime{Manager: manager, Environment: store.HostEnvironment(), UploadDirectory: filepath.Join(paths.State, "image-uploads")}
	browserCache := &imageBrowserCache{}
	hostEnvironment := store.HostEnvironment()
	configured := func(owner providerplugin.ImageOwner) (config.ImageProviderConfiguration, error) {
		value := store.ImageConfiguration()
		if err := value.Validate(); err != nil {
			return config.ImageProviderConfiguration{}, err
		}
		if value != nil {
			if provider, ok := value.Providers[owner.Backend]; ok {
				if provider.Owner != owner {
					return provider, errors.New("configured image owner does not match the requested bundle")
				}
				return provider, nil
			}
			for _, preferred := range value.Preferred {
				if preferred.Backend == owner.Backend && preferred != owner {
					return config.ImageProviderConfiguration{}, errors.New("preferred image owner does not match the requested bundle")
				}
			}
		}
		return config.ImageProviderConfiguration{Owner: owner}, nil
	}
	runtime.ResolveOwner = func(backend string) (providerplugin.ImageOwner, error) {
		value := store.ImageConfiguration()
		if value != nil {
			if provider, ok := value.Providers[backend]; ok {
				return provider.Owner, nil
			}
			for _, owner := range value.Preferred {
				if owner.Backend == backend {
					return owner, nil
				}
			}
		}
		return manager.CaptureImageOwner(backend)
	}
	runtime.Configuration = func(owner providerplugin.ImageOwner) (map[string]any, error) {
		provider, err := configured(owner)
		return provider.Configuration, err
	}
	runtime.ResolveCredentials = func(ctx context.Context, bundle providerplugin.RegisteredImageBundle) (PluginCredentials, error) {
		provider, err := configured(bundle.Owner())
		if err != nil {
			return PluginCredentials{}, err
		}
		selected := bindings
		selected.Providers = provider.Credentials
		if selected.Browser == nil {
			selected.Browser = func(ctx context.Context, declaration manifest.ImageCredential) (http.CookieJar, string, error) {
				return browserCache.resolve(ctx, hostEnvironment, bundle.Owner(), declaration, provider.BrowserProfiles[declaration.ID])
			}
		}
		credentials, err := ResolvePluginCredentials(ctx, store, bundle, selected)
		if err != nil {
			return PluginCredentials{}, err
		}
		validate := credentials.Validate
		credentials.Validate = func() error {
			current, err := configured(bundle.Owner())
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current.Credentials, provider.Credentials) || !reflect.DeepEqual(current.BrowserProfiles, provider.BrowserProfiles) {
				return errors.New("image credential bindings changed during execution")
			}
			return validate()
		}
		return credentials, nil
	}
	runtime.Select = func(ctx context.Context) (providerplugin.ImageOwner, error) {
		value := store.ImageConfiguration()
		if value == nil || len(value.Preferred) == 0 {
			return providerplugin.ImageOwner{}, errors.New("image setup required: configure images.preferred or specify an installed backend")
		}
		if err := value.Validate(); err != nil {
			return providerplugin.ImageOwner{}, err
		}
		for _, owner := range value.Preferred {
			if err := manager.ValidateImageOwner(ctx, owner); err != nil {
				return providerplugin.ImageOwner{}, err
			}
		}
		for _, owner := range value.Preferred {
			bundle, err := manager.ImageBundleForOwner(owner)
			if err != nil {
				return providerplugin.ImageOwner{}, err
			}
			provider, err := configured(owner)
			if err != nil {
				return providerplugin.ImageOwner{}, err
			}
			snapshot := store.RuntimeSnapshot()
			available := true
			for _, credential := range bundle.Manifest.Credentials {
				switch credential.Source {
				case "environment":
					available = available && snapshot.Getenv(credential.Environment) != ""
				case "provider":
					bound, ok := provider.Credentials[credential.ID]
					if !ok || bound.ProviderID != credential.Provider {
						return providerplugin.ImageOwner{}, errors.New("preferred image credential owner is not configured")
					}
					if err := store.ValidateActiveProviderOwner(bound); err != nil {
						return providerplugin.ImageOwner{}, err
					}
					providerConfig, ok := snapshot.Config().Providers.Get(bound.ProviderID)
					if !ok {
						return providerplugin.ImageOwner{}, errors.New("preferred image credential provider is unavailable")
					}
					key, err := snapshot.Resolve(providerConfig.APIKey)
					if err != nil {
						return providerplugin.ImageOwner{}, errors.New("preferred image credential resolution failed")
					}
					available = available && (key != "" || providerConfig.OAuthToken != nil && providerConfig.OAuthToken.AccessToken != "")
				case "browser":
					if bindings.Browser == nil && provider.BrowserProfiles[credential.ID] == "" {
						return providerplugin.ImageOwner{}, errors.New("preferred image browser profile is not configured")
					}
				}
			}
			if available {
				return owner, nil
			}
		}
		return providerplugin.ImageOwner{}, errors.New("configured image providers have no available credentials")
	}
	return runtime, nil
}
