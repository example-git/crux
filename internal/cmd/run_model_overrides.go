package cmd

import (
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/spf13/cobra"
)

// Run flags affect the very first private proposal. Otherwise an unrelated
// default provider could block admission before the requested model is used.
func prepareRunModelOverrides(cmd *cobra.Command) func(*config.ConfigStore) error {
	if cmd.Name() != "run" {
		return nil
	}
	if err := validateRunModelFlags(cmd); err != nil {
		return func(*config.ConfigStore) error { return err }
	}
	large, _ := cmd.Flags().GetString("model")
	small, _ := cmd.Flags().GetString("small-model")
	if large == "" && small == "" {
		return nil
	}
	return func(store *config.ConfigStore) error {
		cfg := store.Config()
		requested, err := resolveModelOverrides(cfg, config.ProviderSurfaces(cfg), large, small, func(providerID string) (config.SelectedModel, error) {
			providers, _ := config.Providers(cfg)
			return config.DefaultSmallModel(cfg, providerID, providers)
		})
		if err != nil {
			return err
		}
		_, err = store.OverrideModelsForOwnersContext(cmd.Context(), requested)
		return err
	}
}

func validateRunModelFlags(cmd *cobra.Command) error {
	for _, name := range []string{"model", "small-model"} {
		if !cmd.Flags().Changed(name) {
			continue
		}
		value, err := cmd.Flags().GetString(name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("--%s requires a model", name)
		}
	}
	return nil
}
