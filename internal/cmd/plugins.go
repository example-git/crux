package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/providerdiagnostics"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/redact"
	"github.com/spf13/cobra"
)

var (
	pluginInstallRef     string
	pluginInstallUpdate  bool
	pluginInstallNoTrust bool
	pluginTrustDigest    string
	pluginTrustRevoke    bool
	pluginOutputJSON     bool
	pluginDiagnoseSource string
	pluginDiagnoseTarget string
	pluginTestAccount    string
	pluginTestChecks     []string
)

var diagnosePluginBundle = func(cmd *cobra.Command, request providerplugin.DiagnoseRequest) (providerplugin.DiagnosticReport, error) {
	manager, err := openPluginManager(cmd)
	if err != nil {
		return providerplugin.DiagnosticReport{}, err
	}
	defer manager.Close()
	return manager.Diagnose(cmd.Context(), request)
}

var loadPluginDiagnosticRuntime = func(cmd *cobra.Command) (providerdiagnostics.Runtime, error) {
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, err
	}
	dataDir, err := cmd.Flags().GetString("data-dir")
	if err != nil {
		return nil, fmt.Errorf("read data directory: %w", err)
	}
	debug, err := cmd.Flags().GetBool("debug")
	if err != nil {
		return nil, fmt.Errorf("read debug flag: %w", err)
	}
	return config.Load(cwd, dataDir, debug)
}

var runLivePluginDiagnostics = providerdiagnostics.Run

var pluginsCmd = &cobra.Command{
	Use:   "plugins",
	Short: "Manage provider plugins",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginsList(cmd)
	},
}

var pluginsBrowserProfilesCmd = &cobra.Command{
	Use:   "browser-profiles",
	Short: "List host browser profile IDs without reading cookies",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		profiles := cookieutil.BrowserProfiles(config.SnapshotEnvironment().Env())
		if pluginOutputJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(profiles)
		}
		for _, profile := range profiles {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", profile.ID, profile.Name)
		}
		return nil
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

var pluginsDiagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Diagnose one provider plugin or preset bundle",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginsDiagnose(cmd)
	},
}

var pluginsTestCmd = &cobra.Command{
	Use:   "test <provider-id>",
	Short: "Test one active provider plugin or preset",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPluginsTest(cmd, args[0])
	},
}

var pluginsRollbackMigrationCmd = &cobra.Command{
	Use:   "rollback-migration",
	Short: "Restore the global provider configuration from migration",
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

func runPluginsDiagnose(cmd *cobra.Command) error {
	report, err := diagnosePluginBundle(cmd, providerplugin.DiagnoseRequest{Source: pluginDiagnoseSource, Target: pluginDiagnoseTarget})
	if err != nil {
		return err
	}
	if err := printPluginDiagnosticReport(cmd, report); err != nil {
		return err
	}
	if !report.Valid {
		return errors.New("provider bundle diagnostics failed")
	}
	return nil
}

func runPluginsTest(cmd *cobra.Command, providerID string) error {
	runtime, err := loadPluginDiagnosticRuntime(cmd)
	if err != nil {
		return err
	}
	checks := make([]providerdiagnostics.Check, len(pluginTestChecks))
	for index, check := range pluginTestChecks {
		checks[index] = providerdiagnostics.Check(strings.TrimSpace(check))
	}
	report, err := runLivePluginDiagnostics(cmd.Context(), runtime, providerdiagnostics.Request{
		ProviderID: providerID,
		AccountID:  pluginTestAccount,
		Checks:     checks,
	})
	if err != nil {
		return err
	}
	if err := printLivePluginDiagnosticReport(cmd, report); err != nil {
		return err
	}
	if !report.Valid {
		return errors.New("provider live diagnostics failed")
	}
	return nil
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

func printPluginDiagnosticReport(cmd *cobra.Command, report providerplugin.DiagnosticReport) error {
	report.PluginType = redact.String(report.PluginType)
	report.ID = redact.String(report.ID)
	report.ProviderID = redact.String(report.ProviderID)
	report.Version = redact.String(report.Version)
	report.Digest = redact.String(report.Digest)
	report.Diagnostics = append([]providerplugin.Diagnostic(nil), report.Diagnostics...)
	for index := range report.Diagnostics {
		report.Diagnostics[index].Code = redact.String(report.Diagnostics[index].Code)
		report.Diagnostics[index].Message = redact.String(report.Diagnostics[index].Message)
		report.Diagnostics[index].Path = redact.String(report.Diagnostics[index].Path)
	}
	if pluginOutputJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	status := "valid"
	if !report.Valid {
		status = "invalid"
	}
	cmd.Printf("Provider bundle diagnostics: %s\n", status)
	if report.PluginType != "" {
		cmd.Printf("  type:     %s\n", report.PluginType)
	}
	if report.ID != "" {
		cmd.Printf("  id:       %s\n", report.ID)
	}
	if report.ProviderID != "" {
		cmd.Printf("  provider: %s\n", report.ProviderID)
	}
	if report.Version != "" {
		cmd.Printf("  version:  %s\n", report.Version)
	}
	if report.Digest != "" {
		cmd.Printf("  digest:   %s\n", report.Digest)
	}
	for _, diagnostic := range report.Diagnostics {
		cmd.Printf("  %s [%s/%s] %s: %s\n", diagnostic.Code, diagnostic.Severity, diagnostic.Phase, diagnostic.Path, diagnostic.Message)
	}
	return nil
}

func printLivePluginDiagnosticReport(cmd *cobra.Command, report providerdiagnostics.Report) error {
	report.ProviderID = redact.String(report.ProviderID)
	report.OwnerType = redact.String(report.OwnerType)
	report.Account.Source = redact.String(report.Account.Source)
	report.Checks = append([]providerdiagnostics.CheckResult(nil), report.Checks...)
	for index := range report.Checks {
		report.Checks[index].Message = redact.String(report.Checks[index].Message)
	}
	report.Operations = append([]providerdiagnostics.OperationResult(nil), report.Operations...)
	for index := range report.Operations {
		report.Operations[index].Path = redact.String(report.Operations[index].Path)
		report.Operations[index].Kind = redact.String(report.Operations[index].Kind)
		report.Operations[index].Message = redact.String(report.Operations[index].Message)
	}
	if pluginOutputJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	status := "passed"
	if !report.Valid {
		status = "failed"
	}
	cmd.Printf("Provider live diagnostics: %s\n", status)
	cmd.Printf("  provider: %s\n", report.ProviderID)
	cmd.Printf("  owner:    %s\n", report.OwnerType)
	if report.OwnerType == string(config.ProviderOwnerPlugin) {
		if report.Account.Loaded {
			cmd.Printf("  account:  loaded (%s)\n", report.Account.Source)
		} else {
			cmd.Println("  account:  unavailable")
		}
	}
	for _, check := range report.Checks {
		cmd.Printf("  check %s: %s (%s)\n", check.Check, check.Status, check.Message)
	}
	for _, operation := range report.Operations {
		if operation.HTTPStatus != 0 {
			cmd.Printf("  operation %s [%s]: %s HTTP %d in %dms (%s)\n", operation.Path, operation.Kind, operation.Status, operation.HTTPStatus, operation.DurationMS, operation.Message)
		} else {
			cmd.Printf("  operation %s [%s]: %s in %dms (%s)\n", operation.Path, operation.Kind, operation.Status, operation.DurationMS, operation.Message)
		}
	}
	return nil
}

func printPluginSnapshot(cmd *cobra.Command, snapshot providerplugin.Snapshot) error {
	redacted := snapshot
	redacted.Plugins = make([]providerplugin.Status, len(snapshot.Plugins))
	for index, status := range snapshot.Plugins {
		redacted.Plugins[index] = status.Clone()
		for diagnosticIndex := range redacted.Plugins[index].Diagnostics {
			redacted.Plugins[index].Diagnostics[diagnosticIndex].Message = redact.String(redacted.Plugins[index].Diagnostics[diagnosticIndex].Message)
		}
	}
	snapshot = redacted
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
		pluginType := plugin.PluginType
		if pluginType == "" {
			pluginType = "unknown"
		}
		cmd.Printf("  type:     %s\n", pluginType)
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
	pluginsDiagnoseCmd.Flags().StringVar(&pluginDiagnoseSource, "source", "", "Local provider plugin or preset bundle directory")
	pluginsDiagnoseCmd.Flags().StringVar(&pluginDiagnoseTarget, "target", "", "Exact installed plugin ID or bundle name")
	pluginsTestCmd.Flags().StringVar(&pluginTestAccount, "account", "", "Exact stored account ID to test instead of the active account")
	pluginsTestCmd.Flags().StringSliceVar(&pluginTestChecks, "check", nil, "Check to run: account, usage, or connection (repeatable)")
	pluginsCmd.PersistentFlags().BoolVar(&pluginOutputJSON, "json", false, "Print plugin output as JSON")
	pluginsCmd.AddCommand(newImageCredentialOwnerCommand(), newImageSetupCommand(), pluginsBrowserProfilesCmd, pluginsListCmd, pluginsInstallCmd, pluginsTrustCmd, pluginsRescanCmd, pluginsDiagnoseCmd, pluginsTestCmd, pluginsRollbackMigrationCmd)
}
