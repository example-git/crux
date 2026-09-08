package imagegen

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

type clientImageSource struct{ snapshot config.RuntimeSnapshot }

func (s clientImageSource) ImageBundleForOwner(owner providerplugin.ImageOwner) (providerplugin.RegisteredImageBundle, error) {
	bundle, handled, err := s.snapshot.ClientImageBundle(owner)
	if !handled {
		return bundle, errors.New("client image authority is unavailable")
	}
	return bundle, err
}

func (s clientImageSource) ValidateImageOwner(ctx context.Context, owner providerplugin.ImageOwner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.ImageBundleForOwner(owner)
	return err
}

func validateImageCredentialOwner(store *config.ConfigStore, snapshot config.RuntimeSnapshot, owner providerregistry.RegistrationOwner) error {
	if !snapshot.IsClientOwned() {
		return store.ValidateActiveProviderOwner(owner)
	}
	actual, ok := snapshot.ProviderOwner(owner.ProviderID)
	if !ok || actual != owner {
		return errors.New("captured client image credential owner is unavailable")
	}
	return nil
}

func newClientPluginRuntime(store *config.ConfigStore) *PluginRuntime {
	runtime := &PluginRuntime{}
	capture := func() *PluginRuntime {
		captured := clientPluginRuntimeSnapshot(store, store.RuntimeSnapshot())
		captured.Client = runtime.Client
		return captured
	}
	runtime.Capture = capture
	runtime.ResolveOwner = func(backend string) (providerplugin.ImageOwner, error) { return capture().ResolveOwner(backend) }
	runtime.Select = func(ctx context.Context) (providerplugin.ImageOwner, error) { return capture().Select(ctx) }
	return runtime
}

func clientPluginRuntimeSnapshot(store *config.ConfigStore, snapshot config.RuntimeSnapshot) *PluginRuntime {
	source := clientImageSource{snapshot: snapshot}
	images := snapshot.Config().Images
	configured := func(owner providerplugin.ImageOwner) (config.ImageProviderConfiguration, error) {
		if _, err := source.ImageBundleForOwner(owner); err != nil {
			return config.ImageProviderConfiguration{}, err
		}
		if images != nil {
			if value, ok := images.Providers[owner.Backend]; ok {
				if value.Owner != owner {
					return value, errors.New("client image configuration owner mismatch")
				}
				return value, nil
			}
		}
		return config.ImageProviderConfiguration{Owner: owner}, nil
	}
	runtime := &PluginRuntime{Source: source, UploadDirectory: filepath.Join(snapshot.Config().Options.DataDirectory, "image-uploads")}
	runtime.ResolveOwner = func(backend string) (providerplugin.ImageOwner, error) {
		if images != nil {
			if value, ok := images.Providers[backend]; ok {
				return value.Owner, nil
			}
			for _, owner := range images.Preferred {
				if owner.Backend == backend {
					return owner, nil
				}
			}
		}
		return providerplugin.ImageOwner{}, errors.New("requested image backend is not selected by the client")
	}
	runtime.Select = func(ctx context.Context) (providerplugin.ImageOwner, error) {
		if images == nil || len(images.Preferred) == 0 {
			return providerplugin.ImageOwner{}, errors.New("select an image backend on the owning client")
		}
		owner := images.Preferred[0]
		return owner, source.ValidateImageOwner(ctx, owner)
	}
	runtime.Configuration = func(owner providerplugin.ImageOwner) (map[string]any, error) {
		value, err := configured(owner)
		return value.Configuration, err
	}
	runtime.ResolveCredentials = func(ctx context.Context, bundle providerplugin.RegisteredImageBundle) (PluginCredentials, error) {
		value, err := configured(bundle.Owner())
		if err != nil {
			return PluginCredentials{}, err
		}
		return resolvePluginCredentialsSnapshot(ctx, store, snapshot, bundle, PluginCredentialBindings{Providers: value.Credentials})
	}
	return runtime
}
