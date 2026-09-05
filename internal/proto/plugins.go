package proto

import "time"

// PluginSnapshot is the authoritative, revisioned view of provider plugins on
// the execution host. It intentionally contains no bundle paths, source URLs,
// credentials, or private manifest data.
type PluginSnapshot struct {
	Profile          string         `json:"profile"`
	EnabledProviders []string       `json:"enabled_providers,omitempty"`
	Revision         uint64         `json:"revision"`
	ScannedAt        time.Time      `json:"scanned_at"`
	Plugins          []PluginStatus `json:"plugins"`
}

// PluginStatus is the redacted client/server representation of one installed
// direct-child provider plugin bundle.
type PluginStatus struct {
	BundleName    string             `json:"bundle_name"`
	PluginType    string             `json:"plugin_type,omitempty"`
	ID            string             `json:"id,omitempty"`
	ProviderID    string             `json:"provider_id,omitempty"`
	Name          string             `json:"name,omitempty"`
	Version       string             `json:"version,omitempty"`
	PublisherID   string             `json:"publisher_id,omitempty"`
	Digest        string             `json:"digest,omitempty"`
	State         string             `json:"state"`
	Trust         string             `json:"trust"`
	Compatibility string             `json:"compatibility"`
	SourceKind    string             `json:"source_kind,omitempty"`
	SourceCommit  string             `json:"source_commit,omitempty"`
	Capabilities  []string           `json:"capabilities,omitempty"`
	Diagnostics   []PluginDiagnostic `json:"diagnostics,omitempty"`
	InstalledAt   time.Time          `json:"installed_at,omitempty"`
}

// PluginDiagnostic is safe for CLI, UI, logs, and remote status surfaces.
type PluginDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
