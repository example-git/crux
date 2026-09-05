package providerplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestDiagnoseSourceReportsSpecificProviderSchemaValues(t *testing.T) {
	manager := newTestManager(t)
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	provider := value["provider"].(map[string]any)
	provider["login_order"] = 0
	secret := strings.Repeat("provider-diagnostic-secret", 64)
	value["id"] = secret
	provider["name"] = secret
	source := filepath.Join(t.TempDir(), "invalid-provider.plugin")
	writeRawBundleManifest(t, source, value)

	report, err := manager.Diagnose(t.Context(), DiagnoseRequest{Source: source})
	require.NoError(t, err)
	require.False(t, report.Valid)
	require.Equal(t, manifest.PluginTypeProvider, report.PluginType)
	require.Empty(t, report.ID)
	requireDiagnosticPath(t, report.Diagnostics, "manifest-schema-invalid", "/id")
	requireDiagnosticPath(t, report.Diagnostics, "manifest-schema-invalid", "/provider/login_order")
	requireDiagnosticPath(t, report.Diagnostics, "manifest-schema-invalid", "/provider/name")
	for _, diagnostic := range report.Diagnostics {
		require.Equal(t, DiagnosticSeverityError, diagnostic.Severity)
		require.Equal(t, DiagnosticPhaseManifest, diagnostic.Phase)
		require.NotContains(t, diagnostic.Message, secret)
		require.LessOrEqual(t, len(diagnostic.Message), MaxDiagnosticBytes+len("…"))
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
}

func TestDiagnoseSourceReportsSpecificPresetSchemaValues(t *testing.T) {
	manager := newTestManager(t)
	data, err := os.ReadFile(filepath.Join(generatedPresetRoot(t), "deepseek.plugin", manifestFilename))
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	preset := value["preset"].(map[string]any)
	preset["api_key"] = "preset-diagnostic-secret"
	models := preset["models"].([]any)
	models[0].(map[string]any)["context_window"] = 0
	source := filepath.Join(t.TempDir(), "invalid-preset.plugin")
	writeRawBundleManifest(t, source, value)

	report, err := manager.Diagnose(t.Context(), DiagnoseRequest{Source: source})
	require.NoError(t, err)
	require.False(t, report.Valid)
	require.Equal(t, manifest.PluginTypeProviderPreset, report.PluginType)
	require.Empty(t, report.ID)
	requireDiagnosticPath(t, report.Diagnostics, "manifest-schema-invalid", "/preset/api_key")
	requireDiagnosticPath(t, report.Diagnostics, "manifest-schema-invalid", "/preset/models/0/context_window")
	for _, diagnostic := range report.Diagnostics {
		require.NotContains(t, diagnostic.Message, "preset-diagnostic-secret")
	}
}

func TestDiagnoseEvaluatesOnlySelectedInstalledTarget(t *testing.T) {
	manager := newTestManager(t)
	value := readExampleManifest(t)
	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, value.ID+bundleSuffix), value)
	other := filepath.Join(manager.paths.Bundles, "other.plugin")
	require.NoError(t, os.MkdirAll(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, manifestFilename), []byte(`{"private":"unselected-diagnostic-secret"}`), 0o600))
	_, err := manager.Rescan(t.Context(), manager.Snapshot().Revision)
	require.NoError(t, err)

	report, err := manager.Diagnose(t.Context(), DiagnoseRequest{Target: value.ID})
	require.NoError(t, err)
	require.True(t, report.Valid)
	require.Equal(t, value.ID, report.ID)
	require.Empty(t, report.Diagnostics)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "unselected-diagnostic-secret")
}

func TestInstallUsesActivationDiagnosticsBeforeCommit(t *testing.T) {
	manager := newTestManager(t)
	value := activationInvalidManifest(t)
	source := filepath.Join(t.TempDir(), value.ID+bundleSuffix)
	writeBundleManifest(t, source, value)

	_, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true})
	var diagnosticError *DiagnosticError
	require.ErrorAs(t, err, &diagnosticError)
	require.False(t, diagnosticError.Report.Valid)
	requireDiagnosticPath(t, diagnosticError.Report.Diagnostics, "activation-invalid", "/capabilities/runtime_controls/0/scope")
	_, statErr := os.Stat(filepath.Join(manager.paths.Bundles, value.ID+bundleSuffix))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRescanDoesNotRunSelectiveActivationDiagnostics(t *testing.T) {
	manager := newTestManager(t)
	value := activationInvalidManifest(t)
	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, value.ID+bundleSuffix), value)

	snapshot, err := manager.Rescan(t.Context(), manager.Snapshot().Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	require.Equal(t, StateUntrusted, snapshot.Plugins[0].State)
	require.NotContains(t, diagnosticCodes(snapshot.Plugins[0]), "activation-invalid")
}

func activationInvalidManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	value := readExampleManifest(t)
	value.ID = "activation-invalid"
	value.Provider.ID = "activation-invalid"
	value.Provider.AccountNamespace = "activation-invalid"
	value.Capabilities.RuntimeControls = []manifest.RuntimeControl{{
		ID:          "invalid_scope",
		Label:       "Invalid scope",
		Type:        "boolean",
		Scope:       "request",
		RequestPath: "/invalid_scope",
	}}
	require.NoError(t, manifest.Validate(value))
	return value
}

func writeRawBundleManifest(t *testing.T, root string, value map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o700))
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, manifestFilename), data, 0o600))
}

func requireDiagnosticPath(t *testing.T, diagnostics []Diagnostic, code, path string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Path == path {
			return
		}
	}
	t.Fatalf("missing diagnostic %s at %s: %#v", code, path, diagnostics)
}
