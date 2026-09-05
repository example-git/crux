// Package manifest defines the versioned, provider-neutral contract for
// installable provider plugin bundles. It does not discover, trust, or execute
// bundles; those responsibilities belong to the plugin host.
package manifest

import "encoding/json"

const (
	// Version is the only manifest schema major supported by this package.
	Version = 1
	// HostAPIVersion is the provider capability API implemented by this schema.
	HostAPIVersion = 1

	PluginTypeProvider       = "provider"
	PluginTypeProviderPreset = "provider-preset"
)

// Manifest is stored as manifest.json at the root of a direct *.plugin bundle.
type Manifest struct {
	Schema          string                     `json:"$schema,omitempty" jsonschema:"format=uri-reference,description=Optional editor schema hint; ignored for compatibility"`
	PluginType      string                     `json:"plugin_type,omitempty" jsonschema:"enum=provider,description=Bundle contract type; omitted manifests are legacy provider bundles"`
	ManifestVersion int                        `json:"manifest_version" jsonschema:"required,minimum=1,maximum=1,description=Manifest schema major version"`
	ID              string                     `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$,maxLength=128,description=Stable plugin identity"`
	Version         string                     `json:"version" jsonschema:"required,pattern=^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\\+[0-9A-Za-z.-]+)?$,maxLength=128,description=Plugin semantic version without a v prefix"`
	Name            string                     `json:"name" jsonschema:"required,minLength=1,maxLength=128,description=Human-readable plugin name"`
	Description     string                     `json:"description" jsonschema:"required,minLength=1,maxLength=1024,description=Human-readable plugin purpose"`
	Publisher       Publisher                  `json:"publisher" jsonschema:"required,description=Publisher identity displayed during trust decisions"`
	Compatibility   Compatibility              `json:"compatibility" jsonschema:"required,description=Host API and feature compatibility"`
	Provider        Provider                   `json:"provider" jsonschema:"required,description=Logical provider registered by this bundle"`
	Models          []Model                    `json:"models" jsonschema:"required,minItems=1,maxItems=512,description=Closed model catalog in stable display order"`
	Capabilities    Capabilities               `json:"capabilities" jsonschema:"required,description=Provider capabilities and declarative behavior"`
	Configuration   Configuration              `json:"configuration" jsonschema:"required,description=Provider configuration schema and presentation hints"`
	Extensions      map[string]json.RawMessage `json:"extensions,omitempty" jsonschema:"description=Opaque namespaced data values; keys must begin with x-"`
}

type Publisher struct {
	ID      string `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$,maxLength=128"`
	Name    string `json:"name" jsonschema:"required,minLength=1,maxLength=128"`
	URL     string `json:"url,omitempty" jsonschema:"format=uri,maxLength=2048"`
	Support string `json:"support_url,omitempty" jsonschema:"format=uri,maxLength=2048"`
}

type Compatibility struct {
	HostAPI          VersionBounds `json:"host_api" jsonschema:"required"`
	HostVersion      *SemverBounds `json:"host_version,omitempty"`
	RequiredFeatures []string      `json:"required_features,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
	OptionalFeatures []string      `json:"optional_features,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
}

type VersionBounds struct {
	Min int `json:"min" jsonschema:"required,minimum=1"`
	Max int `json:"max" jsonschema:"required,minimum=1"`
}

type SemverBounds struct {
	Min string `json:"min,omitempty" jsonschema:"pattern=^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\\+[0-9A-Za-z.-]+)?$"`
	Max string `json:"max,omitempty" jsonschema:"pattern=^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\\+[0-9A-Za-z.-]+)?$"`
}

type Provider struct {
	ID                   string   `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$,maxLength=64"`
	Name                 string   `json:"name" jsonschema:"required,minLength=1,maxLength=128"`
	Description          string   `json:"description,omitempty" jsonschema:"maxLength=1024"`
	Aliases              []string `json:"aliases,omitempty" jsonschema:"uniqueItems=true,maxItems=16"`
	AccountNamespace     string   `json:"account_namespace" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:[-_.][a-z0-9]+)*$,maxLength=128"`
	LegacyAccountAliases []string `json:"legacy_account_aliases,omitempty" jsonschema:"uniqueItems=true,maxItems=16"`
	DefaultLargeModel    string   `json:"default_large_model" jsonschema:"required,minLength=1,maxLength=256"`
	DefaultSmallModel    string   `json:"default_small_model" jsonschema:"required,minLength=1,maxLength=256"`
	LoginOrder           int      `json:"login_order" jsonschema:"required,minimum=1,maximum=10000"`
	AccountOrder         int      `json:"account_order" jsonschema:"required,minimum=1,maximum=10000"`
	FlatRate             bool     `json:"flat_rate,omitempty"`
	Brand                *Brand   `json:"brand,omitempty"`
}

type Brand struct {
	Label     string `json:"label,omitempty" jsonschema:"maxLength=64"`
	ShortName string `json:"short_name,omitempty" jsonschema:"maxLength=24"`
	Color     string `json:"color,omitempty" jsonschema:"pattern=^#[0-9A-Fa-f]{6}$"`
	GradientA string `json:"gradient_a,omitempty" jsonschema:"pattern=^#[0-9A-Fa-f]{6}$"`
	GradientB string `json:"gradient_b,omitempty" jsonschema:"pattern=^#[0-9A-Fa-f]{6}$"`
}

type Model struct {
	ID                 string                     `json:"id" jsonschema:"required,minLength=1,maxLength=256"`
	Name               string                     `json:"name" jsonschema:"required,minLength=1,maxLength=256"`
	CostPer1MIn        float64                    `json:"cost_per_1m_in,omitempty" jsonschema:"minimum=0"`
	CostPer1MOut       float64                    `json:"cost_per_1m_out,omitempty" jsonschema:"minimum=0"`
	CostPer1MInCached  float64                    `json:"cost_per_1m_in_cached,omitempty" jsonschema:"minimum=0"`
	CostPer1MOutCached float64                    `json:"cost_per_1m_out_cached,omitempty" jsonschema:"minimum=0"`
	ContextWindow      int64                      `json:"context_window" jsonschema:"required,minimum=1"`
	DefaultMaxTokens   int64                      `json:"default_max_tokens" jsonschema:"required,minimum=1"`
	Reasoning          *Reasoning                 `json:"reasoning,omitempty"`
	Modalities         Modalities                 `json:"modalities" jsonschema:"required"`
	DefaultOptions     map[string]any             `json:"default_options,omitempty"`
	Extensions         map[string]json.RawMessage `json:"extensions,omitempty"`
}

type Reasoning struct {
	Levels  []string `json:"levels,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
	Default string   `json:"default,omitempty" jsonschema:"maxLength=64"`
	Budgets *Bounds  `json:"budgets,omitempty"`
}

type Bounds struct {
	Min int64 `json:"min,omitempty"`
	Max int64 `json:"max,omitempty"`
}

type Modalities struct {
	Input  []string `json:"input" jsonschema:"required,minItems=1,uniqueItems=true,enum=text,enum=image,enum=audio,enum=video,enum=document"`
	Output []string `json:"output" jsonschema:"required,minItems=1,uniqueItems=true,enum=text,enum=image,enum=audio"`
}

type Configuration struct {
	Schema map[string]any          `json:"schema" jsonschema:"required,description=Draft 2020-12 JSON Schema for provider configuration"`
	Fields map[string]FieldDisplay `json:"fields,omitempty" jsonschema:"description=Generic presentation hints keyed by configuration property"`
}

type FieldDisplay struct {
	Label       string `json:"label" jsonschema:"required,maxLength=128"`
	Description string `json:"description,omitempty" jsonschema:"maxLength=1024"`
	Secret      bool   `json:"secret,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
	Order       int    `json:"order,omitempty"`
}

type Capabilities struct {
	Compatibility    *CompatibilityAdapter             `json:"compatibility_adapter,omitempty"`
	Credentials      []Credential                      `json:"credentials,omitempty" jsonschema:"maxItems=32"`
	OAuth            []OAuthFlow                       `json:"oauth,omitempty" jsonschema:"maxItems=16"`
	ClientIdentities map[string]ResolvedClientIdentity `json:"client_identities,omitempty"`
	Endpoints        []Endpoint                        `json:"endpoints" jsonschema:"required,minItems=1,maxItems=64"`
	Headers          []HeaderRule                      `json:"headers,omitempty" jsonschema:"maxItems=256"`
	JSONTransforms   map[string]JSONPipeline           `json:"json_transforms,omitempty"`
	PromptTransforms map[string]PromptPipeline         `json:"prompt_transforms,omitempty"`
	RoleMaps         map[string]RoleMap                `json:"role_maps,omitempty"`
	ToolCodecs       map[string]ToolCodec              `json:"tool_codecs,omitempty"`
	Operations       []Operation                       `json:"operations" jsonschema:"required,minItems=1,maxItems=64"`
	Usage            *UsagePolicy                      `json:"usage,omitempty"`
	Images           *ImagePolicy                      `json:"images,omitempty"`
	Instructions     *InstructionPolicy                `json:"instructions,omitempty"`
	RuntimeControls  []RuntimeControl                  `json:"runtime_controls,omitempty" jsonschema:"maxItems=64"`
	Metadata         []MetadataContract                `json:"metadata,omitempty" jsonschema:"maxItems=64"`
	Errors           []ErrorMapping                    `json:"errors,omitempty" jsonschema:"maxItems=256"`
	Anthropic        *AnthropicPolicy                  `json:"anthropic,omitempty"`
}

// CompatibilityAdapter explicitly delegates finite capability groups to a
// host-known integrated adapter during migration. Inventory entries assign
// every remaining behavior to either a proposed bounded core primitive or an
// explicit private/stateful compatibility boundary; bundles never provide code.
type CompatibilityAdapter struct {
	ID        string                       `json:"id" jsonschema:"required,pattern=^integrated-[a-z][a-z0-9-]*$,maxLength=128"`
	Delegates []string                     `json:"delegates" jsonschema:"required,minItems=1,uniqueItems=true,maxItems=8,enum=construction,enum=oauth,enum=identity,enum=usage,enum=runtime,enum=reasoning"`
	Inventory []CompatibilityInventoryItem `json:"inventory" jsonschema:"required,minItems=1,uniqueItems=true,maxItems=128"`
}

// CompatibilityInventoryItem makes temporary adapter ownership reviewable and
// machine-checkable without introducing an executable plugin extension.
type CompatibilityInventoryItem struct {
	Delegate       string `json:"delegate" jsonschema:"required,enum=construction,enum=oauth,enum=identity,enum=usage,enum=runtime,enum=reasoning"`
	Classification string `json:"classification" jsonschema:"required,enum=finite-core-primitive,enum=private-stateful"`
	Behavior       string `json:"behavior" jsonschema:"required,minLength=1,maxLength=1024"`
	Primitive      string `json:"primitive,omitempty" jsonschema:"maxLength=256"`
}
