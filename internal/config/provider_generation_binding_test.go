package config

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderBehaviorRegistrationStaysBoundToConfigGeneration(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	const providerID = "same-provider"
	const pluginID = "same.plugin"
	registration := func(hiddenSkill string, maxSide int) providerregistry.Registration {
		images := &manifest.ImagePolicy{
			AcceptedMediaTypes: []string{"image/jpeg"},
			MaxSourceBytes:     1024,
			MaxSidePixels:      maxSide,
		}
		return providerregistry.Registration{
			ProviderID: providerID,
			Manifest: &manifest.Manifest{
				ID:      pluginID,
				Version: "1.0.0",
				Configuration: manifest.Configuration{Schema: map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				}},
				Capabilities: manifest.Capabilities{Images: images},
			},
			Images:       images,
			Instructions: &providerregistry.InstructionCapability{HiddenSkills: []string{hiddenSkill}},
		}
	}
	oldRegistry, err := providerregistry.New(registration("old-skill", 100))
	require.NoError(t, err)
	newRegistry, err := providerregistry.New(registration("new-skill", 200))
	require.NoError(t, err)
	newConfig := func(registry *providerregistry.Registry) *Config {
		cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
			providerID: {Plugin: &ProviderPluginReference{ID: pluginID, Version: "1.0.0"}},
		})}
		cfg.bindProviderScan(ProviderScan{Registry: registry})
		return cfg
	}
	oldConfig := newConfig(oldRegistry)
	newConfigGeneration := newConfig(newRegistry)
	providerOnce.Do(func() {})
	publishProviderScan(ProviderScan{Registry: newRegistry}, nil)

	oldBehavior, ok := oldConfig.ProviderBehaviorRegistration(providerID)
	require.True(t, ok)
	require.Equal(t, []string{"old-skill"}, oldBehavior.Instructions.HiddenSkills)
	require.Equal(t, 100, oldBehavior.Images.MaxSidePixels)

	newBehavior, ok := newConfigGeneration.ProviderBehaviorRegistration(providerID)
	require.True(t, ok)
	require.Equal(t, []string{"new-skill"}, newBehavior.Instructions.HiddenSkills)
	require.Equal(t, 200, newBehavior.Images.MaxSidePixels)
}

func TestWorkspaceProviderPublicationCannotReplaceAccountGenerations(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	const firstProvider = "workspace-account-first"
	const secondProvider = "workspace-account-second"
	refresher := func(value string) accounts.Refresher {
		return func(context.Context, string) (*oauth.Token, error) {
			return &oauth.Token{AccessToken: value}, nil
		}
	}
	accounts.PublishProviders([]accounts.ProviderRegistration{
		{ProviderID: firstProvider, Namespace: "compatibility-first", Refresher: refresher("compatibility-first")},
		{ProviderID: secondProvider, Namespace: "compatibility-second", Refresher: refresher("compatibility-second")},
	})
	t.Cleanup(func() { accounts.PublishProviders(nil) })
	newRegistration := func(providerID, namespace, alias, value string, order int) providerregistry.Registration {
		return providerregistry.Registration{
			ProviderID:       providerID,
			AccountNamespace: namespace,
			AccountAliases:   []string{alias},
			AccountOrder:     order,
			Construction:     providerregistry.ConstructionGenericJSON,
			OAuth:            &providerregistry.OAuthCapability{Refresh: refresher(value)},
		}
	}
	workspaceA := []providerregistry.Registration{
		newRegistration(firstProvider, "workspace-a-later", "workspace-a-first-alias", "workspace-a-later", 20),
		newRegistration(secondProvider, "workspace-a-earlier", "workspace-a-second-alias", "workspace-a-earlier", 10),
	}
	workspaceB := []providerregistry.Registration{
		newRegistration(firstProvider, "workspace-b-earlier", "workspace-b-first-alias", "workspace-b-earlier", 10),
		newRegistration(secondProvider, "workspace-b-later", "workspace-b-second-alias", "workspace-b-later", 20),
	}
	registryA, err := providerregistry.New(workspaceA...)
	require.NoError(t, err)
	registryB, err := providerregistry.New(workspaceB...)
	require.NoError(t, err)
	configFor := func(registry *providerregistry.Registry) *Config {
		cfg := &Config{}
		cfg.bindProviderScan(ProviderScan{Registry: registry})
		return cfg
	}
	configA := configFor(registryA)
	configB := configFor(registryB)
	unboundConfig := &Config{}
	require.Equal(t, []string{"workspace-a-earlier", "workspace-a-later"}, configA.ProviderAccountNamespaces())
	require.Equal(t, []string{"workspace-b-earlier", "workspace-b-later"}, configB.ProviderAccountNamespaces())

	validateUnbound := func() error {
		if namespaces := unboundConfig.ProviderAccountNamespaces(); len(namespaces) != 0 {
			return fmt.Errorf("unbound configuration borrowed account namespaces %v", namespaces)
		}
		for _, name := range []string{
			firstProvider,
			secondProvider,
			"workspace-a-later",
			"workspace-a-first-alias",
			"workspace-b-earlier",
			"workspace-b-first-alias",
		} {
			if registration, ok := unboundConfig.ProviderRegistrationForAccount(name); ok {
				return fmt.Errorf("unbound configuration resolved account %q to provider %q", name, registration.ProviderID)
			}
		}
		return nil
	}

	validate := func(cfg *Config, registrations []providerregistry.Registration) error {
		for _, expected := range registrations {
			actual, ok := cfg.ProviderRegistration(expected.ProviderID)
			if !ok {
				return fmt.Errorf("provider %s is unavailable", expected.ProviderID)
			}
			if actual.AccountNamespace != expected.AccountNamespace || actual.OAuth == nil || actual.OAuth.Refresh == nil {
				return fmt.Errorf("provider %s resolved account namespace %q", expected.ProviderID, actual.AccountNamespace)
			}
			token, err := actual.OAuth.Refresh(context.Background(), "refresh")
			if err != nil || token.AccessToken != expected.AccountNamespace {
				return fmt.Errorf("provider %s resolved the wrong refresher", expected.ProviderID)
			}
			byAlias, ok := cfg.ProviderRegistrationForAccount(expected.AccountAliases[0])
			if !ok || byAlias.ProviderID != expected.ProviderID || byAlias.AccountNamespace != expected.AccountNamespace {
				return fmt.Errorf("provider %s resolved the wrong account alias", expected.ProviderID)
			}
		}
		for _, expected := range []struct {
			providerID string
			namespace  string
		}{
			{providerID: firstProvider, namespace: "compatibility-first"},
			{providerID: secondProvider, namespace: "compatibility-second"},
		} {
			namespace, refresh, ok := accounts.ProviderSnapshot(expected.providerID)
			if !ok || namespace != expected.namespace || refresh == nil {
				return fmt.Errorf("compatibility provider %s resolved namespace %q", expected.providerID, namespace)
			}
			token, err := refresh(context.Background(), "refresh")
			if err != nil || token.AccessToken != expected.namespace {
				return fmt.Errorf("compatibility provider %s resolved the wrong refresher", expected.providerID)
			}
		}
		return nil
	}

	var wait sync.WaitGroup
	errors := make(chan error, 8)
	for worker := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := range 250 {
				if (worker+iteration)%2 == 0 {
					publishProviderScan(ProviderScan{Registry: registryA}, nil)
				} else {
					publishProviderScan(ProviderScan{Registry: registryB}, nil)
				}
				if err := validate(configA, workspaceA); err != nil {
					errors <- err
					return
				}
				if err := validate(configB, workspaceB); err != nil {
					errors <- err
					return
				}
				if err := validateUnbound(); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
}
