package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/spf13/cobra"
)

var (
	pluginInstallRef     string
	pluginInstallUpdate  bool
	pluginInstallNoTrust bool
	pluginTrustDigest    string
	pluginTrustRevoke    bool
	pluginOutputJSON     bool
)

var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage provider plugins",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginsList(cmd)
	},
}

var pluginsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed provider plugins",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginsList(cmd)
	},
}

var pluginsInstallCmd = &cobra.Command{
	Use:   "install <directory-or-https-git-url>",
	Short: "Install a provider plugin from a directory or HTTPS Git repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := openPluginManager(cmd)
		if err != nil {
			return err
		}
		defer manager.Close()
		snapshot, err := manager.Install(cmd.Context(), providerplugin.InstallRequest{
			Source: args[0],
			Ref:    pluginInstallRef,
			Update: pluginInstallUpdate,
			Trust:  !pluginInstallNoTrust,
		})
		if err != nil {
			return err
		}
		return printPluginSnapshot(cmd, snapshot)
	},
}

var pluginsTrustCmd = &cobra.Command{
	Use:   "trust <plugin-id>",
	Short: "Approve or revoke one exact installed plugin digest",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := openPluginManager(cmd)
		if err != nil {
			return err
		}
		defer manager.Close()
		digest := strings.ToLower(strings.TrimSpace(pluginTrustDigest))
		if len(digest) != 64 {
			return errors.New("--digest must be the exact 64-character SHA-256 shown by `crux plugins list`")
		}
		snapshot, err := manager.SetTrust(cmd.Context(), args[0], providerplugin.TrustRequest{
			Digest:  digest,
			Trusted: !pluginTrustRevoke,
		})
		if err != nil {
			return err
		}
		return printPluginSnapshot(cmd, snapshot)
	},
}

var pluginsRollbackMigrationCmd = &cobra.Command{
	Use:   "rollback-migration",
	Short: "Restore the provider configuration and account backup from migration",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.RollbackProviderMigration(); err != nil {
			return err
		}
		cmd.Println("Provider migration rolled back. Restart Crux to reload the restored state.")
		return nil
	},
}

var pluginsRescanCmd = &cobra.Command{
	Use:   "rescan",
	Short: "Rescan canonical installed plugin bundles without network access",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := openPluginManager(cmd)
		if err != nil {
			return err
		}
		defer manager.Close()
		snapshot, err := manager.Rescan(cmd.Context(), manager.Snapshot().Revision)
		if err != nil {
			return err
		}
		return printPluginSnapshot(cmd, snapshot)
	},
}

func openPluginManager(cmd *cobra.Command) (*providerplugin.Manager, error) {
	manager, err := providerplugin.NewManager(cmd.Context(), providerplugin.DefaultPaths(config.GlobalWorkspaceDir(), config.GlobalCacheDir()))
	if err != nil {
		return nil, fmt.Errorf("initialize provider plugins: %w", err)
	}
	return manager, nil
}

func runPluginsList(cmd *cobra.Command) error {
	manager, err := openPluginManager(cmd)
	if err != nil {
		return err
	}
	defer manager.Close()
	return printPluginSnapshot(cmd, manager.Snapshot())
}

func printPluginSnapshot(cmd *cobra.Command, snapshot providerplugin.Snapshot) error {
	profile, enabled, err := config.EffectiveProviderRollout()
	if err != nil {
		return err
	}
	if pluginOutputJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			Profile          string   `json:"profile"`
			EnabledProviders []string `json:"enabled_providers,omitempty"`
			providerplugin.Snapshot
		}{Profile: string(profile), EnabledProviders: enabled, Snapshot: snapshot})
	}
	cmd.Printf("Provider profile: %s", profile)
	if len(enabled) > 0 {
		cmd.Printf(" (enabled: %s)", strings.Join(enabled, ", "))
	}
	cmd.Println()
	if len(snapshot.Plugins) == 0 {
		cmd.Println("No provider plugins installed. Core-only mode is available.")
		return nil
	}
	for _, plugin := range snapshot.Plugins {
		identity := plugin.ID
		if identity == "" {
			identity = plugin.BundleName
		}
		version := ""
		if plugin.Version != "" {
			version = " " + plugin.Version
		}
		cmd.Printf("%s%s  %s  trust=%s\n", identity, version, plugin.State, plugin.Trust)
		if plugin.ProviderID != "" {
			cmd.Printf("  provider: %s\n", plugin.ProviderID)
		}
		if plugin.Digest != "" {
			cmd.Printf("  digest:   %s\n", plugin.Digest)
		}
		if plugin.SourceKind == "git" && plugin.SourceCommit != "" {
			cmd.Printf("  source:   git commit %s\n", plugin.SourceCommit)
		} else if plugin.SourceKind != "" {
			cmd.Printf("  source:   %s\n", plugin.SourceKind)
		}
		if len(plugin.Capabilities) > 0 {
			cmd.Printf("  capabilities: %s\n", strings.Join(plugin.Capabilities, ", "))
		}
		for _, diagnostic := range plugin.Diagnostics {
			cmd.Printf("  %s: %s\n", diagnostic.Code, diagnostic.Message)
		}
	}
	return nil
}

func init() {
	pluginsInstallCmd.Flags().StringVar(&pluginInstallRef, "ref", "", "Git branch, tag, or commit to resolve exactly")
	pluginsInstallCmd.Flags().BoolVar(&pluginInstallUpdate, "update", false, "Replace an existing plugin after complete validation")
	pluginsInstallCmd.Flags().BoolVar(&pluginInstallNoTrust, "no-trust", false, "Install for inspection without trusting or activating the exact digest")
	pluginsTrustCmd.Flags().StringVar(&pluginTrustDigest, "digest", "", "Exact installed bundle SHA-256 digest")
	pluginsTrustCmd.Flags().BoolVar(&pluginTrustRevoke, "revoke", false, "Revoke trust for the exact digest")
	pluginsCmd.PersistentFlags().BoolVar(&pluginOutputJSON, "json", false, "Print revisioned plugin status as JSON")
	pluginsCmd.AddCommand(pluginsListCmd, pluginsInstallCmd, pluginsTrustCmd, pluginsRescanCmd, pluginsRollbackMigrationCmd)
}
