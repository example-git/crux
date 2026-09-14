package proto

import "github.com/example-git/crux/internal/config"

const (
	RemoteRuntimeProtocol           = "crux-client-runtime-v1"
	RemoteRuntimeLocalSharing       = "local-client"
	RemoteRuntimeCertificateSharing = "exclusive-certificate"
)

// CreateWorkspaceRequest is private input, distinct from public discovery.
type CreateWorkspaceRequest struct {
	Workspace
	AuthorityMode string                        `json:"authority_mode,omitempty"`
	Runtime       *config.RemoteRuntimeProposal `json:"runtime,omitempty"`
}

type UpdateRemoteRuntimeRequest struct {
	ExpectedRevision uint64                       `json:"expected_revision"`
	Runtime          config.RemoteRuntimeProposal `json:"runtime"`
}

// RemoteRuntimeCapabilities contains no account, provider configuration or token.
type RemoteRuntimeCapabilities struct {
	CodebaseIndex         bool   `json:"codebase_index,omitempty"`
	Protocol              string `json:"protocol"`
	RuntimeVersion        int    `json:"runtime_version"`
	Compiler              string `json:"compiler"`
	HostVersion           string `json:"host_version"`
	MaxRequestBytes       int    `json:"max_request_bytes"`
	MaxBundles            int    `json:"max_bundles"`
	MaxProviders          int    `json:"max_providers"`
	Principal             string `json:"principal"`
	WorkspaceSharing      string `json:"workspace_sharing"`
	DisconnectGraceMillis int64  `json:"disconnect_grace_millis"`
}
