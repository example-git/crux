package manifest

// AnthropicPolicy configures finite Anthropic Messages wire transformations.
// It is interpreted by the host and contains no executable behavior.
type AnthropicPolicy struct {
	ClientIdentity       *ResolvedClientIdentity `json:"client_identity,omitempty"`
	SessionHeader        string                  `json:"session_header,omitempty" jsonschema:"maxLength=128"`
	DeleteHeaderPrefixes []string                `json:"delete_header_prefixes,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
	DeleteHeaders        []string                `json:"delete_headers,omitempty" jsonschema:"uniqueItems=true,maxItems=64"`
	MaxRequestBytes      int64                   `json:"max_request_bytes" jsonschema:"required,minimum=1,maximum=16777216"`
	TransformFailure     string                  `json:"transform_failure" jsonschema:"required,enum=error,enum=warn-original"`
	SystemLinePrefixes   []string                `json:"system_line_prefixes,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
	SystemPrefixes       []string                `json:"system_prefixes,omitempty" jsonschema:"maxItems=16"`
	MetadataUserID       bool                    `json:"metadata_user_id,omitempty"`
	Billing              *BillingAttribution     `json:"billing,omitempty"`
	ReasoningFallback    bool                    `json:"reasoning_fallback,omitempty"`
	StreamHoldbackBytes  int                     `json:"stream_holdback_bytes,omitempty" jsonschema:"minimum=64,maximum=65536"`
}

// ResolvedClientIdentity resolves a validated client version in a fixed order:
// environment, bounded HTTPS probe, persisted cache, then literal fallback.
type ResolvedClientIdentity struct {
	Environment     string `json:"environment,omitempty" jsonschema:"maxLength=128"`
	LatestURL       string `json:"latest_url,omitempty" jsonschema:"format=uri,maxLength=2048"`
	CacheKey        string `json:"cache_key" jsonschema:"required,pattern=^[a-z][a-z0-9_-]*$,maxLength=64"`
	FallbackVersion string `json:"fallback_version" jsonschema:"required,maxLength=64"`
	VersionPattern  string `json:"version_pattern" jsonschema:"required,maxLength=256"`
	UserAgentFormat string `json:"user_agent_format" jsonschema:"required,maxLength=256"`
	ProbeTimeoutMS  int    `json:"probe_timeout_ms" jsonschema:"required,minimum=1,maximum=30000"`
	ProbeMaxBytes   int64  `json:"probe_max_bytes" jsonschema:"required,minimum=1,maximum=65536"`
}

// BillingAttribution derives a bounded fingerprint from the first user text.
type BillingAttribution struct {
	Salt        string `json:"salt" jsonschema:"required,maxLength=256"`
	ByteOffsets []int  `json:"byte_offsets" jsonschema:"required,minItems=1,maxItems=16"`
	MissingByte string `json:"missing_byte" jsonschema:"required,minLength=1,maxLength=1"`
	HashPrefix  int    `json:"hash_prefix" jsonschema:"required,minimum=1,maximum=64"`
	Format      string `json:"format" jsonschema:"required,maxLength=512"`
}
