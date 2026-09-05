package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/spf13/cobra"
)

func newImageCredentialOwnerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "image-credential-owner <provider-id>",
		Short: "Print the active non-secret credential owner for image configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("host") || cmd.Flags().Changed("connection") {
				return fmt.Errorf("image credential ownership must be inspected on the execution host")
			}
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			dataDir, err := cmd.Flags().GetString("data-dir")
			if err != nil {
				return err
			}
			debug, err := cmd.Flags().GetBool("debug")
			if err != nil {
				return err
			}
			store, err := config.LoadIsolated(cwd, dataDir, debug, config.SnapshotEnvironment())
			if err != nil {
				return err
			}
			snapshot := store.RuntimeSnapshot()
			provider, ok := snapshot.Config().Providers.Get(args[0])
			if !ok {
				return fmt.Errorf("credential provider is not configured")
			}
			owner, ok := snapshot.ProviderOwnerFor(args[0], provider)
			if !ok {
				return fmt.Errorf("credential provider owner is unavailable")
			}
			if err := store.ValidateActiveProviderOwner(owner); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(owner)
		},
	}
}

func newImageSetupCommand() *cobra.Command {
	var digest, configurationPath string
	var update bool
	command := &cobra.Command{
		Use:   "setup-images <local-bundle-directory>",
		Short: "Preview or explicitly install a distinct image provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("host") || cmd.Flags().Changed("connection") {
				return fmt.Errorf("image setup must run directly on the execution host; remote setup transport is unavailable")
			}
			if digest == "" {
				manager, err := openPluginManager(cmd)
				if err != nil {
					return err
				}
				defer manager.Close()
				bundle, err := manager.InspectImageSource(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				owner := bundle.Owner()
				if pluginOutputJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"owner": owner, "installed": false, "requires_digest_consent": true})
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Plugin: %s\nVersion: %s\nBackend: %s\nSHA-256: %s\nNot installed. Repeat with --digest %s to install and trust these exact bytes.\n", owner.PluginID, owner.Version, owner.Backend, owner.Digest, owner.Digest)
				return err
			}
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			dataDir, err := cmd.Flags().GetString("data-dir")
			if err != nil {
				return err
			}
			debug, err := cmd.Flags().GetBool("debug")
			if err != nil {
				return err
			}
			store, err := config.LoadIsolated(cwd, dataDir, debug, config.SnapshotEnvironment())
			if err != nil {
				return err
			}
			runtime, err := imagegen.NewHostPluginRuntime(cmd.Context(), store, imagegen.PluginCredentialBindings{})
			if err != nil {
				return err
			}
			defer runtime.Manager.Close()
			bundle, err := runtime.Manager.InspectImageSource(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			service := &imagegen.SetupService{Runtime: runtime, Store: store}
			install := service.Install
			if update {
				install = service.Update
			}
			if bundle.Owner().Digest != digest {
				return fmt.Errorf("image setup digest does not match the inspected bundle")
			}
			if err := install(cmd.Context(), args[0], digest, configurationPath); err != nil {
				return err
			}
			if pluginOutputJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"owner": store.ImageConfiguration().Providers[bundle.Owner().Backend].Owner, "installed": true, "generation_started": false})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Image provider installed and configured. No generation was started.")
			return err
		},
	}
	command.Flags().BoolVar(&update, "update", false, "Explicitly update the same configured image plugin to the consented digest")
	command.Flags().StringVar(&digest, "digest", "", "Exact SHA-256 from the preview; explicitly consents to installation and trust")
	command.Flags().StringVar(&configurationPath, "configuration", "", "Absolute private JSON configuration path; do not pass secrets as arguments")
	return command
}
