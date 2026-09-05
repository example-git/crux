package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerdiagnostics"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/redact"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestPluginsCommandRegistered(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"plugins", "install"})
	require.NoError(t, err)
	require.Same(t, pluginsInstallCmd, command)
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("ref"))
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("update"))
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("no-trust"))
	require.NotNil(t, pluginsTrustCmd.Flags().Lookup("digest"))
	require.NotNil(t, pluginsTrustCmd.Flags().Lookup("revoke"))
	command, _, err = rootCmd.Find([]string{"plugins", "diagnose"})
	require.NoError(t, err)
	require.Same(t, pluginsDiagnoseCmd, command)
	require.NotNil(t, pluginsDiagnoseCmd.Flags().Lookup("source"))
	require.NotNil(t, pluginsDiagnoseCmd.Flags().Lookup("target"))
	command, _, err = rootCmd.Find([]string{"plugins", "test"})
	require.NoError(t, err)
	require.Same(t, pluginsTestCmd, command)
	require.NotNil(t, pluginsTestCmd.Flags().Lookup("account"))
	require.NotNil(t, pluginsTestCmd.Flags().Lookup("check"))
}

func TestRunPluginsDiagnoseForwardsSelectionAndPrintsFailure(t *testing.T) {
	secret := "private-static-diagnostic"
	redact.Register(secret)
	previousRunner := diagnosePluginBundle
	previousSource := pluginDiagnoseSource
	previousTarget := pluginDiagnoseTarget
	previousJSON := pluginOutputJSON
	t.Cleanup(func() {
		diagnosePluginBundle = previousRunner
		pluginDiagnoseSource = previousSource
		pluginDiagnoseTarget = previousTarget
		pluginOutputJSON = previousJSON
	})
	pluginDiagnoseSource = "selected-source"
	pluginDiagnoseTarget = ""
	pluginOutputJSON = false
	diagnosePluginBundle = func(_ *cobra.Command, request providerplugin.DiagnoseRequest) (providerplugin.DiagnosticReport, error) {
		require.Equal(t, providerplugin.DiagnoseRequest{Source: "selected-source"}, request)
		return providerplugin.DiagnosticReport{Diagnostics: []providerplugin.Diagnostic{{Code: "manifest-invalid", Path: "/provider/id", Message: secret}}}, nil
	}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)

	err := runPluginsDiagnose(command)
	require.ErrorContains(t, err, "provider bundle diagnostics failed")
	require.Contains(t, output.String(), "Provider bundle diagnostics: invalid")
	require.Contains(t, output.String(), "manifest-invalid")
	require.Contains(t, output.String(), redact.Replacement)
	require.NotContains(t, output.String(), secret)
}

func TestRunPluginsTestForwardsExactSelectionAndPrintsFailure(t *testing.T) {
	previousLoader := loadPluginDiagnosticRuntime
	previousRunner := runLivePluginDiagnostics
	previousAccount := pluginTestAccount
	previousChecks := pluginTestChecks
	previousJSON := pluginOutputJSON
	t.Cleanup(func() {
		loadPluginDiagnosticRuntime = previousLoader
		runLivePluginDiagnostics = previousRunner
		pluginTestAccount = previousAccount
		pluginTestChecks = previousChecks
		pluginOutputJSON = previousJSON
	})
	pluginTestAccount = "private-selected-account"
	pluginTestChecks = []string{"account", "usage"}
	pluginOutputJSON = false
	loadPluginDiagnosticRuntime = func(*cobra.Command) (providerdiagnostics.Runtime, error) { return nil, nil }
	runLivePluginDiagnostics = func(_ context.Context, runtime providerdiagnostics.Runtime, request providerdiagnostics.Request) (providerdiagnostics.Report, error) {
		require.Nil(t, runtime)
		require.Equal(t, providerdiagnostics.Request{
			ProviderID: "selected-provider", AccountID: "private-selected-account",
			Checks: []providerdiagnostics.Check{providerdiagnostics.CheckAccount, providerdiagnostics.CheckUsage},
		}, request)
		return providerdiagnostics.Report{
			ProviderID: "selected-provider", OwnerType: "plugin",
			Checks: []providerdiagnostics.CheckResult{{Check: providerdiagnostics.CheckAccount, Status: providerdiagnostics.StatusFailed, Message: "authenticated account could not be loaded"}},
		}, nil
	}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)

	err := runPluginsTest(command, "selected-provider")
	require.ErrorContains(t, err, "provider live diagnostics failed")
	require.Contains(t, output.String(), "Provider live diagnostics: failed")
	require.NotContains(t, output.String(), "private-selected-account")
}

func TestDiagnosticReportPrintersRedactTextAndJSONWithoutMutation(t *testing.T) {
	secret := "private-diagnostic-output"
	redact.Register(secret)
	static := providerplugin.DiagnosticReport{
		Valid: true, PluginType: "provider", ID: "synthetic.plugin", ProviderID: "synthetic", Version: "1.0.0", Digest: strings.Repeat("a", 64),
		Diagnostics: []providerplugin.Diagnostic{{Code: "synthetic", Severity: providerplugin.DiagnosticSeverityError, Phase: providerplugin.DiagnosticPhaseManifest, Path: "/provider/id", Message: secret}},
	}
	live := providerdiagnostics.Report{
		Valid: true, ProviderID: "synthetic", OwnerType: "plugin", Account: providerdiagnostics.AccountResult{Loaded: true, Source: "stored"},
		Checks:     []providerdiagnostics.CheckResult{{Check: providerdiagnostics.CheckUsage, Status: providerdiagnostics.StatusPassed, Message: secret}},
		Operations: []providerdiagnostics.OperationResult{{Path: "/capabilities/operations/2", Kind: "custom", Status: providerdiagnostics.StatusPassed, HTTPStatus: 200, DurationMS: 1, Message: secret}},
	}
	previousJSON := pluginOutputJSON
	t.Cleanup(func() { pluginOutputJSON = previousJSON })
	for _, jsonOutput := range []bool{false, true} {
		pluginOutputJSON = jsonOutput
		for _, print := range []func(*cobra.Command) error{
			func(command *cobra.Command) error { return printPluginDiagnosticReport(command, static) },
			func(command *cobra.Command) error { return printLivePluginDiagnosticReport(command, live) },
		} {
			var output bytes.Buffer
			command := &cobra.Command{}
			command.SetOut(&output)
			require.NoError(t, print(command))
			require.NotContains(t, output.String(), secret)
			require.Contains(t, output.String(), redact.Replacement)
		}
	}
	require.Equal(t, secret, static.Diagnostics[0].Message)
	require.Equal(t, secret, live.Checks[0].Message)
	require.Equal(t, secret, live.Operations[0].Message)
}

func TestPluginsRollbackMigrationHelpDescribesGlobalProviderConfigOnly(t *testing.T) {
	var output bytes.Buffer
	pluginsRollbackMigrationCmd.SetOut(&output)
	t.Cleanup(func() { pluginsRollbackMigrationCmd.SetOut(nil) })

	require.NoError(t, pluginsRollbackMigrationCmd.Help())
	help := strings.ToLower(output.String())
	require.Contains(t, help, "restore the global provider configuration from migration")
	require.NotContains(t, help, "account")
	require.NotContains(t, help, "backup")
}

func TestPrintEmptyPluginSnapshot(t *testing.T) {
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	previous := pluginOutputJSON
	pluginOutputJSON = false
	t.Cleanup(func() { pluginOutputJSON = previous })
	require.NoError(t, printPluginSnapshot(command, providerplugin.Snapshot{}))
	require.Contains(t, output.String(), "Core-only mode is available")
}

func TestPrintPluginSnapshotRedactsDiagnosticsWithoutMutatingSnapshot(t *testing.T) {
	secret := "cli-plugin-diagnostic-secret-value"
	redact.Register(secret)
	snapshot := providerplugin.Snapshot{Plugins: []providerplugin.Status{{
		BundleName:  "example.plugin",
		Diagnostics: []providerplugin.Diagnostic{{Code: "invalid", Message: "failed " + secret}},
	}}}
	previous := pluginOutputJSON
	t.Cleanup(func() { pluginOutputJSON = previous })
	for _, jsonOutput := range []bool{false, true} {
		var output bytes.Buffer
		command := &cobra.Command{}
		command.SetOut(&output)
		pluginOutputJSON = jsonOutput
		require.NoError(t, printPluginSnapshot(command, snapshot))
		require.NotContains(t, output.String(), secret)
		require.Contains(t, output.String(), redact.Replacement)
	}
	require.Contains(t, snapshot.Plugins[0].Diagnostics[0].Message, secret)
}

func TestPrintPluginSnapshotIncludesBundleType(t *testing.T) {
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	previous := pluginOutputJSON
	pluginOutputJSON = false
	t.Cleanup(func() { pluginOutputJSON = previous })

	snapshot := providerplugin.Snapshot{Plugins: []providerplugin.Status{
		{BundleName: "example.provider.plugin", PluginType: "provider", ID: "example.provider", State: providerplugin.StateRegistered, Trust: providerplugin.TrustTrusted},
		{BundleName: "example.preset.plugin", PluginType: "provider-preset", ID: "example.preset", State: providerplugin.StateRegistered, Trust: providerplugin.TrustTrusted},
	}}
	require.NoError(t, printPluginSnapshot(command, snapshot))

	text := output.String()
	require.Contains(t, text, "example.provider  registered  trust=trusted\n  type:     provider\n")
	require.Contains(t, text, "example.preset  registered  trust=trusted\n  type:     provider-preset\n")
}
