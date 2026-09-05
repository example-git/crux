package providerplugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
)

type DiagnoseRequest struct {
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
}

type DiagnosticReport struct {
	Valid       bool         `json:"valid"`
	PluginType  string       `json:"plugin_type,omitempty"`
	ID          string       `json:"id,omitempty"`
	ProviderID  string       `json:"provider_id,omitempty"`
	Version     string       `json:"version,omitempty"`
	Digest      string       `json:"digest,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

type DiagnosticError struct {
	Report DiagnosticReport
}

func (e *DiagnosticError) Error() string {
	if e == nil || len(e.Report.Diagnostics) == 0 {
		return "provider plugin diagnostics failed"
	}
	diagnostic := e.Report.Diagnostics[0]
	if diagnostic.Path != "" {
		return fmt.Sprintf("provider plugin diagnostics failed: %s at %s", diagnostic.Code, diagnostic.Path)
	}
	return fmt.Sprintf("provider plugin diagnostics failed: %s", diagnostic.Code)
}

func (m *Manager) Diagnose(ctx context.Context, request DiagnoseRequest) (DiagnosticReport, error) {
	source := strings.TrimSpace(request.Source)
	target := strings.TrimSpace(request.Target)
	if (source == "") == (target == "") {
		return DiagnosticReport{}, errors.New("exactly one plugin source or installed target is required")
	}
	if source != "" {
		if strings.Contains(source, "://") {
			return DiagnosticReport{}, errors.New("diagnostic plugin source must be a local directory")
		}
		absolute, err := filepath.Abs(source)
		if err != nil {
			return DiagnosticReport{}, errors.New("resolve diagnostic plugin source")
		}
		return m.diagnosePath(ctx, absolute)
	}

	lockContext, cancel := context.WithTimeout(ctx, managerLockTimeout)
	defer cancel()
	release, err := lock.File(lockContext, m.paths.ManagerLock)
	if err != nil {
		return DiagnosticReport{}, fmt.Errorf("lock plugin diagnostics: %w", err)
	}
	defer release()

	m.mu.RLock()
	var selected *Status
	for index := range m.state.Plugins {
		status := m.state.Plugins[index]
		if status.ID != target && status.BundleName != target {
			continue
		}
		if selected != nil {
			m.mu.RUnlock()
			return DiagnosticReport{}, errors.New("installed plugin target is ambiguous")
		}
		selected = &status
	}
	m.mu.RUnlock()
	if selected == nil {
		return DiagnosticReport{}, ErrPluginMissing
	}
	path := selected.path
	if path == "" {
		path = filepath.Join(m.paths.Bundles, selected.BundleName)
	}
	return m.diagnosePath(ctx, path)
}

func (m *Manager) diagnosePath(ctx context.Context, source string) (DiagnosticReport, error) {
	if err := ctx.Err(); err != nil {
		return DiagnosticReport{}, err
	}
	temporary := filepath.Join(m.paths.Cache, ".diagnose-"+uuid.NewString())
	snapshot, err := snapshotDirectory(source, temporary)
	if err != nil {
		_ = os.RemoveAll(temporary)
		message := safeDiagnostic("bundle-snapshot-invalid", err.Error()).Message
		return DiagnosticReport{}, fmt.Errorf("snapshot selected plugin bundle: %s", message)
	}
	defer os.RemoveAll(temporary)
	report, _ := diagnoseSnapshot(temporary, snapshot)
	return report, nil
}

func diagnoseSnapshot(root string, snapshot snapshotResult) (DiagnosticReport, validatedBundle) {
	report := DiagnosticReport{Digest: snapshot.Digest}
	manifestPresent := false
	for _, file := range snapshot.Files {
		if file.Path == manifestFilename {
			manifestPresent = true
			break
		}
	}
	if !manifestPresent {
		report.Diagnostics = []Diagnostic{safeDetailedDiagnostic("manifest-missing", DiagnosticPhaseBundle, "/", "bundle root does not contain manifest.json")}
		return report, validatedBundle{}
	}
	data, err := readBoundedRegularFile(filepath.Join(root, manifestFilename), manifest.MaxManifestBytes)
	if err != nil {
		report.Diagnostics = []Diagnostic{safeDetailedDiagnostic("manifest-read-failed", DiagnosticPhaseBundle, "/", "manifest could not be read securely")}
		return report, validatedBundle{}
	}
	pluginType, err := manifest.DecodePluginType(data)
	if err != nil {
		report.Diagnostics = []Diagnostic{safeDetailedDiagnostic("manifest-invalid", DiagnosticPhaseManifest, "/plugin_type", "manifest type is invalid")}
		return report, validatedBundle{}
	}
	report.PluginType = pluginType

	var schemaPaths []string
	switch pluginType {
	case manifest.PluginTypeProvider:
		schemaPaths, err = manifest.ProviderSchemaIssuePaths(data)
	case manifest.PluginTypeProviderPreset:
		schemaPaths, err = manifest.PresetSchemaIssuePaths(data)
	case manifest.PluginTypeImageProvider:
		schemaPaths, err = manifest.ImageSchemaIssuePaths(data)
	}
	if err != nil {
		report.Diagnostics = []Diagnostic{safeDetailedDiagnostic("manifest-schema-unavailable", DiagnosticPhaseManifest, "/", "host could not evaluate the manifest schema")}
		return report, validatedBundle{}
	}
	for _, path := range schemaPaths {
		report.Diagnostics = append(report.Diagnostics, safeDetailedDiagnostic("manifest-schema-invalid", DiagnosticPhaseManifest, path, "value does not satisfy the provider bundle schema"))
	}
	if len(report.Diagnostics) > 0 {
		return report, validatedBundle{}
	}

	validated, diagnostics := validateSnapshot(root, snapshot)
	if len(diagnostics) > 0 {
		for _, diagnostic := range diagnostics {
			report.Diagnostics = append(report.Diagnostics, safeDetailedDiagnostic(diagnostic.Code, DiagnosticPhaseManifest, semanticDiagnosticPath(diagnostic.Message), "manifest declaration is invalid"))
		}
		return report, validatedBundle{}
	}
	report.ID = validated.id()
	report.ProviderID = validated.providerID()
	report.Version = validated.version()

	for _, diagnostic := range compatibilityDiagnostics(validated.compatibility()) {
		report.Diagnostics = append(report.Diagnostics, safeDetailedDiagnostic(diagnostic.Code, DiagnosticPhaseCompatibility, "/compatibility", "bundle is incompatible with this host"))
	}
	if validated.manifest != nil {
		registration, err := providerregistry.FromManifest(*validated.manifest, validated.staticText)
		if err == nil {
			err = providerregistry.ValidateActivation(registration)
		}
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, safeDetailedDiagnostic("activation-invalid", DiagnosticPhaseActivation, activationDiagnosticPath(*validated.manifest, err), "declaration is not executable by the selected host construction"))
		}
	}
	report.Valid = len(report.Diagnostics) == 0
	return report, validated
}

func semanticDiagnosticPath(message string) string {
	field := strings.Fields(message)
	if len(field) == 0 {
		return "/"
	}
	value := strings.Trim(field[0], ":")
	value = strings.ReplaceAll(value, ".", "/")
	value = strings.ReplaceAll(value, "[", "/")
	value = strings.ReplaceAll(value, "]", "")
	if value == "" || strings.ContainsAny(value, "\"'") {
		return "/"
	}
	return "/" + strings.TrimPrefix(value, "/")
}

func activationDiagnosticPath(value manifest.Manifest, err error) string {
	message := err.Error()
	for index, control := range value.Capabilities.RuntimeControls {
		if !strings.Contains(message, fmt.Sprintf("%q", control.ID)) {
			continue
		}
		path := fmt.Sprintf("/capabilities/runtime_controls/%d", index)
		switch {
		case strings.Contains(message, "scope"):
			return path + "/scope"
		case strings.Contains(message, "request path"):
			return path + "/request_path"
		default:
			return path
		}
	}
	for index, operation := range value.Capabilities.Operations {
		if strings.Contains(message, fmt.Sprintf("%q", operation.ID)) {
			return fmt.Sprintf("/capabilities/operations/%d", index)
		}
	}
	return "/capabilities"
}
