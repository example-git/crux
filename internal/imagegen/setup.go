package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/question"
)

var ErrSetupRequired = errors.New("image setup required: run crux plugins setup-images /absolute/path/to/provider.plugin on the execution host to preview the bundle, then repeat with its --digest and --configuration file to install, trust, and configure it")

type SetupRequest struct {
	SessionID   string
	ToolCallID  string
	Interactive bool
}

type SetupService struct {
	Runtime         *PluginRuntime
	Store           *config.ConfigStore
	Questions       question.Service
	browserProfiles func() []cookieutil.BrowserProfile
	mu              sync.Mutex
	flight          *imageSetupFlight
}

type imageSetupFlight struct {
	done chan struct{}
	err  error
}

func (s *SetupService) Ensure(ctx context.Context, request SetupRequest) error {
	if s == nil || s.Runtime == nil || s.Runtime.Manager == nil || s.Store == nil {
		return ErrSetupRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if flight := s.flight; flight != nil {
		s.mu.Unlock()
		if !request.Interactive {
			return ErrSetupRequired
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-flight.done:
			return flight.err
		}
	}
	flight := &imageSetupFlight{done: make(chan struct{})}
	s.flight = flight
	s.mu.Unlock()
	flight.err = s.ensure(ctx, request)
	s.mu.Lock()
	s.flight = nil
	close(flight.done)
	s.mu.Unlock()
	return flight.err
}

func (s *SetupService) ensure(ctx context.Context, request SetupRequest) error {
	manager := s.Runtime.Manager
	if _, err := manager.Rescan(ctx, 0); err != nil {
		return err
	}
	bundles, err := manager.RegisteredImageBundles()
	if err != nil {
		return err
	}
	if len(bundles) > 0 {
		return nil
	}
	expected := s.Store.ImageConfiguration()
	if expected != nil && (len(expected.Preferred) > 0 || len(expected.Providers) > 0) {
		for _, owner := range expected.Preferred {
			if _, err := manager.ImageBundleForOwner(owner); err != nil {
				return fmt.Errorf("image setup cannot replace a configured owner: %w", err)
			}
		}
		return errors.New("configured image owners are unavailable; inspect crux plugins list and repair the selected owner's installation or trust before retrying setup")
	}
	for _, status := range manager.Snapshot().Plugins {
		if status.PluginType == manifest.PluginTypeImageProvider {
			return fmt.Errorf("image provider %s is unavailable (state: %s, trust: %s); inspect crux plugins list before setup", status.ID, status.State, status.Trust)
		}
	}
	if !request.Interactive || s.Questions == nil || request.SessionID == "" {
		return ErrSetupRequired
	}
	ask := func(id string, kind question.Type, text, description string) (question.Answer, error) {
		answers, err := s.Questions.Ask(ctx, question.Request{SessionID: request.SessionID, ToolCallID: request.ToolCallID, Questions: []question.Question{{ID: id, Type: kind, Text: text, Description: description}}})
		if errors.Is(err, question.ErrCancelled) {
			return question.Answer{}, ErrSetupRequired
		}
		if err != nil {
			return question.Answer{}, err
		}
		if len(answers) != 1 || answers[0].QuestionID != id {
			return question.Answer{}, errors.New("image setup received an invalid answer")
		}
		return answers[0], nil
	}
	answer, err := ask("image-setup", question.TypeYesNo, "Set up an image provider?", "No image providers are installed. Setup installs only a bundle you choose; generation still requires its normal permission afterward.")
	if err != nil {
		return err
	}
	if answer.Yes == nil || !*answer.Yes {
		return ErrSetupRequired
	}
	answer, err = ask("image-source", question.TypeFreeText, "Where is the image-provider bundle?", "Enter an absolute local bundle-directory path on the execution host. No provider recipes or credentials are downloaded automatically.")
	if err != nil {
		return err
	}
	source := answer.FillInText
	bundle, err := manager.InspectImageSource(ctx, source)
	if err != nil {
		return err
	}
	owner := bundle.Owner()
	answer, err = ask("image-trust", question.TypeYesNo, "Install and trust this exact image bundle?", fmt.Sprintf("Plugin: %s\nVersion: %s\nBackend: %s\nSHA-256: %s", owner.PluginID, owner.Version, owner.Backend, owner.Digest))
	if err != nil {
		return err
	}
	if answer.Yes == nil || !*answer.Yes {
		return ErrSetupRequired
	}
	provider := config.ImageProviderConfiguration{Owner: owner}
	_, _, configurationErr := imageConfiguration(bundle.Manifest, nil)
	needsConfiguration := configurationErr != nil
	for _, credential := range bundle.Manifest.Credentials {
		needsConfiguration = needsConfiguration || credential.Source == "provider"
	}
	if needsConfiguration {
		answer, err = ask("image-configuration", question.TypeFreeText, "Where is the private image configuration file?", "Enter an absolute local JSON path containing configuration and exact-owner credentials bindings. Use existing provider login for authentication. Do not enter secrets in this answer.")
		if err != nil {
			return err
		}
		provider, err = readImageSetupConfiguration(answer.FillInText, owner)
		if err != nil {
			return err
		}
	}
	if _, err := s.selectBrowserProfiles(ctx, request, bundle, &provider); err != nil {
		return err
	}
	return s.install(ctx, source, bundle, provider, expected, false, false)
}

func (s *SetupService) Install(ctx context.Context, source, digest, configurationPath string) error {
	return s.installSource(ctx, source, digest, configurationPath, false)
}

func (s *SetupService) Update(ctx context.Context, source, digest, configurationPath string) error {
	return s.installSource(ctx, source, digest, configurationPath, true)
}

func (s *SetupService) installSource(ctx context.Context, source, digest, configurationPath string, update bool) error {
	if s == nil || s.Runtime == nil || s.Runtime.Manager == nil || s.Store == nil {
		return ErrSetupRequired
	}
	if digest == "" {
		return errors.New("image setup requires explicit exact-digest consent")
	}
	bundle, err := s.Runtime.Manager.InspectImageSource(ctx, source)
	if err != nil {
		return err
	}
	if bundle.Owner().Digest != digest {
		return errors.New("image setup digest does not match the inspected bundle")
	}
	expected := s.Store.ImageConfiguration()
	provider := config.ImageProviderConfiguration{Owner: bundle.Owner()}
	if update {
		if expected == nil {
			return errors.New("image update requires a configured owner")
		}
		existing, ok := expected.Providers[bundle.Owner().Backend]
		if !ok || existing.Owner.PluginID != bundle.Owner().PluginID {
			return errors.New("image update requires the same configured plugin identity")
		}
		provider = existing
		provider.Owner = bundle.Owner()
	}
	if configurationPath != "" {
		provider, err = readImageSetupConfiguration(configurationPath, bundle.Owner())
		if err != nil {
			return err
		}
	}
	return s.install(ctx, source, bundle, provider, expected, true, update)
}

func (s *SetupService) install(ctx context.Context, source string, bundle providerplugin.RegisteredImageBundle, provider config.ImageProviderConfiguration, expected *config.ImageConfiguration, additional, update bool) error {
	manager := s.Runtime.Manager
	owner := bundle.Owner()
	if expected != nil {
		if !additional && (len(expected.Preferred) > 0 || len(expected.Providers) > 0) {
			return errors.New("image setup will not replace configured image owners")
		}
		if existing, exists := expected.Providers[owner.Backend]; exists && (!update || existing.Owner.PluginID != owner.PluginID) {
			return errors.New("image setup will not replace a configured image owner")
		}
		for _, preferred := range expected.Preferred {
			if preferred.Backend == owner.Backend && (!update || preferred.PluginID != owner.PluginID) {
				return errors.New("image setup will not replace a preferred image owner")
			}
		}
	}
	if _, err := manager.Rescan(ctx, 0); err != nil {
		return err
	}
	for _, status := range manager.Snapshot().Plugins {
		if status.PluginType == manifest.PluginTypeImageProvider && (!additional || status.ProviderID == owner.Backend || status.ID == owner.PluginID) {
			if update && status.ID == owner.PluginID && status.ProviderID == owner.Backend {
				continue
			}
			return errors.New("image setup will not replace installed image providers")
		}
	}
	if _, _, err := imageConfiguration(bundle.Manifest, provider.Configuration); err != nil {
		return err
	}
	value := &config.ImageConfiguration{Providers: map[string]config.ImageProviderConfiguration{}}
	if expected != nil {
		value.Preferred = append(value.Preferred, expected.Preferred...)
		for backend, existing := range expected.Providers {
			value.Providers[backend] = existing
		}
	}
	replaced := false
	for index, preferred := range value.Preferred {
		if preferred.Backend == owner.Backend {
			value.Preferred[index] = owner
			replaced = true
		}
	}
	if !replaced {
		value.Preferred = append(value.Preferred, owner)
	}
	value.Providers[owner.Backend] = provider
	if err := value.Validate(); err != nil {
		return err
	}
	for _, credential := range bundle.Manifest.Credentials {
		if credential.Source == "browser" && provider.BrowserProfiles[credential.ID] == "" {
			return fmt.Errorf("image setup credential %q requires a browser_profiles binding; list profiles with crux plugins browser-profiles and add the chosen ID to the --configuration file", credential.ID)
		}
		if credential.Source == "provider" {
			bound, ok := provider.Credentials[credential.ID]
			if !ok || bound.ProviderID != credential.Provider {
				return fmt.Errorf("image setup credential %q requires an exact owner binding for provider %q; obtain it with crux plugins image-credential-owner %s and add it to credentials in the --configuration file", credential.ID, credential.Provider, credential.Provider)
			}
			if err := s.Store.ValidateActiveProviderOwner(bound); err != nil {
				return fmt.Errorf("image setup credential %q for provider %q is unavailable: %w", credential.ID, credential.Provider, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := manager.Install(ctx, providerplugin.InstallRequest{Source: source, ExpectedDigest: owner.Digest, Trust: true, Update: update}); err != nil {
		return err
	}
	if err := manager.ValidateImageOwner(ctx, owner); err != nil {
		return err
	}
	return s.Store.CompareAndSetImageConfiguration(expected, value)
}

func readImageSetupConfiguration(path string, owner providerplugin.ImageOwner) (config.ImageProviderConfiguration, error) {
	if !filepath.IsAbs(path) {
		return config.ImageProviderConfiguration{}, errors.New("image configuration path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return config.ImageProviderConfiguration{}, fmt.Errorf("cannot inspect image configuration file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return config.ImageProviderConfiguration{}, errors.New("image configuration must be a regular JSON file no larger than 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return config.ImageProviderConfiguration{}, fmt.Errorf("cannot open image configuration file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return config.ImageProviderConfiguration{}, errors.New("image configuration file exceeds its bound or cannot be read")
	}
	var value config.ImageProviderConfiguration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return value, fmt.Errorf("invalid image configuration JSON syntax at byte %d", syntax.Offset)
		}
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			return value, fmt.Errorf("invalid image configuration field %q: expected %s at byte %d", mismatch.Field, mismatch.Type, mismatch.Offset)
		}
		return value, errors.New("invalid image configuration JSON: expected one object using only owner, configuration, credentials, and browser_profiles fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return value, errors.New("image configuration must contain one JSON value")
	}
	if value.Owner != (providerplugin.ImageOwner{}) && value.Owner != owner {
		return value, errors.New("image configuration file belongs to a different owner")
	}
	value.Owner = owner
	return value, nil
}
