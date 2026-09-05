package providerplugin

import (
	"maps"
	"slices"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
)

// State is the lifecycle state of one discovered bundle.
type State string

const (
	StateDiscovered   State = "discovered"
	StateInvalid      State = "invalid"
	StateIncompatible State = "incompatible"
	StateUntrusted    State = "untrusted"
	StateDisabled     State = "disabled"
	StateRegistered   State = "registered"
	StateQuarantined  State = "quarantined"
)

// TrustState describes whether the exact immutable bundle digest is approved.
type TrustState string

const (
	TrustUnknown TrustState = "unknown"
	TrustTrusted TrustState = "trusted"
	TrustRevoked TrustState = "revoked"
)

// CompatibilityState describes host compatibility independently from trust.
type CompatibilityState string

const (
	CompatibilityUnknown      CompatibilityState = "unknown"
	CompatibilityCompatible   CompatibilityState = "compatible"
	CompatibilityIncompatible CompatibilityState = "incompatible"
)

type DiagnosticSeverity string

const (
	DiagnosticSeverityError DiagnosticSeverity = "error"
)

type DiagnosticPhase string

const (
	DiagnosticPhaseBundle        DiagnosticPhase = "bundle"
	DiagnosticPhaseManifest      DiagnosticPhase = "manifest"
	DiagnosticPhaseCompatibility DiagnosticPhase = "compatibility"
	DiagnosticPhaseActivation    DiagnosticPhase = "activation"
)

// Diagnostic is safe to expose through logs, CLI, UI, and client/server APIs.
// Message is bounded and scrubbed before construction; it never contains raw
// plugin stderr, credentials, source URLs, or absolute source/cache paths.
type Diagnostic struct {
	Code     string             `json:"code"`
	Message  string             `json:"message"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Phase    DiagnosticPhase    `json:"phase,omitempty"`
	Path     string             `json:"path,omitempty"`
}

// Status is the redacted status of one installed direct-child bundle.
type Status struct {
	BundleName    string             `json:"bundle_name"`
	PluginType    string             `json:"plugin_type,omitempty"`
	ID            string             `json:"id,omitempty"`
	ProviderID    string             `json:"provider_id,omitempty"`
	Name          string             `json:"name,omitempty"`
	Version       string             `json:"version,omitempty"`
	PublisherID   string             `json:"publisher_id,omitempty"`
	Digest        string             `json:"digest,omitempty"`
	State         State              `json:"state"`
	Trust         TrustState         `json:"trust"`
	Compatibility CompatibilityState `json:"compatibility"`
	SourceKind    string             `json:"source_kind,omitempty"`
	SourceCommit  string             `json:"source_commit,omitempty"`
	Capabilities  []string           `json:"capabilities,omitempty"`
	Diagnostics   []Diagnostic       `json:"diagnostics,omitempty"`
	InstalledAt   time.Time          `json:"installed_at,omitempty"`

	manifest   *manifest.Manifest
	preset     *manifest.PresetManifest
	image      *manifest.ImageManifest
	staticText map[string]string
	path       string
}

// Snapshot is the immutable authoritative view of host plugins.
type Snapshot struct {
	Revision  uint64    `json:"revision"`
	ScannedAt time.Time `json:"scanned_at"`
	Plugins   []Status  `json:"plugins"`
}

func (s Snapshot) clone() Snapshot {
	result := s
	result.Plugins = make([]Status, len(s.Plugins))
	for i := range s.Plugins {
		result.Plugins[i] = s.Plugins[i]
		result.Plugins[i].Capabilities = slices.Clone(s.Plugins[i].Capabilities)
		result.Plugins[i].Diagnostics = slices.Clone(s.Plugins[i].Diagnostics)
		for index := range result.Plugins[i].Diagnostics {
			result.Plugins[i].Diagnostics[index].Message = redact.String(result.Plugins[i].Diagnostics[index].Message)
		}
		result.Plugins[i].staticText = maps.Clone(s.Plugins[i].staticText)
		if s.Plugins[i].manifest != nil {
			value := *s.Plugins[i].manifest
			result.Plugins[i].manifest = &value
		}
		if s.Plugins[i].preset != nil {
			value := *s.Plugins[i].preset
			result.Plugins[i].preset = &value
		}
		result.Plugins[i].image = nil
	}
	return result
}

func (s Status) Clone() Status {
	return Snapshot{Plugins: []Status{s}}.clone().Plugins[0]
}

// Event notifies consumers that an authoritative snapshot revision changed.
type Event struct {
	Revision   uint64   `json:"revision"`
	Kind       string   `json:"kind"`
	ChangedIDs []string `json:"changed_ids,omitempty"`
}

// InstallRequest describes one explicit local-directory or HTTPS Git install.
type InstallRequest struct {
	ExpectedDigest   string `json:"expected_digest,omitempty"`
	Source           string `json:"source"`
	Ref              string `json:"ref,omitempty"`
	Update           bool   `json:"update,omitempty"`
	Trust            bool   `json:"trust,omitempty"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`

	sourceKind   string
	sourceCommit string
}

// TrustRequest approves or revokes one exact digest.
type TrustRequest struct {
	Digest           string `json:"digest"`
	Trusted          bool   `json:"trusted"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`
}
