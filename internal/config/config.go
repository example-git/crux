package config

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/discover"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
)

const (
	appName              = "crux"
	defaultDataDirectory = ".crux"
	defaultInitializeAs  = "AGENTS.md"

	ToolingInstructionsCrux   = "crux"
	ToolingInstructionsNative = "native"
)

// defaultContextPaths are project-local context files loaded into the
// system prompt. The ~/.ai-cli/ project-specific instructions are added
// dynamically in setDefaults() since they depend on the working directory.
var defaultContextPaths = []string{
	"AGENTS.md",
	"agents.md",
	"Agents.md",
	"CRUX.md",
	"CRUX.local.md",
	"Crux.md",
	"Crux.local.md",
	"crux.md",
	"crux.local.md",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	"gemini.md",
	".github/copilot-instructions.md",
	".cursorrules",
	".cursor/rules/",
}

type SelectedModelType string

// String returns the string representation of the [SelectedModelType].
func (s SelectedModelType) String() string {
	return string(s)
}

const (
	SelectedModelTypeLarge SelectedModelType = "large"
	SelectedModelTypeSmall SelectedModelType = "small"
)

const (
	AgentCoder string = "coder"
	AgentTask  string = "task"
)

type SelectedModel struct {
	// The model id as used by the provider API.
	// Required.
	Model string `json:"model" jsonschema:"required,description=The model ID as used by the provider API,example=gpt-4o"`
	// The model provider, same as the key/id used in the providers config.
	// Required.
	Provider string `json:"provider" jsonschema:"required,description=The model provider ID that matches a key in the providers config,example=openai"`

	// Only used by models that use the openai provider and need this set.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort level for OpenAI models that support it,enum=low,enum=medium,enum=high"`

	// Enables provider-specific reasoning when the selected model supports it.
	Think bool `json:"think,omitempty" jsonschema:"description=Enable provider reasoning mode when supported"`

	// Overrides the default model configuration.
	MaxTokens        int64    `json:"max_tokens,omitempty" jsonschema:"description=Maximum number of tokens for model responses,maximum=200000,example=4096"`
	Temperature      *float64 `json:"temperature,omitempty" jsonschema:"description=Sampling temperature,minimum=0,maximum=1,example=0.7"`
	TopP             *float64 `json:"top_p,omitempty" jsonschema:"description=Top-p (nucleus) sampling parameter,minimum=0,maximum=1,example=0.9"`
	TopK             *int64   `json:"top_k,omitempty" jsonschema:"description=Top-k sampling parameter"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty" jsonschema:"description=Frequency penalty to reduce repetition"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty" jsonschema:"description=Presence penalty to increase topic diversity"`

	// Override provider specific options.
	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for the model"`
}

type OwnedSelectedModel struct {
	Model SelectedModel                      `json:"model"`
	Owner providerregistry.RegistrationOwner `json:"owner"`
}

type AgentModelState struct {
	Large *OwnedSelectedModel `json:"large,omitempty"`
	Small *OwnedSelectedModel `json:"small,omitempty"`
}

type ProviderOwnerType string

const (
	ProviderOwnerCore   ProviderOwnerType = "core"
	ProviderOwnerCustom ProviderOwnerType = "custom"
	ProviderOwnerPlugin ProviderOwnerType = "plugin"
	ProviderOwnerPreset ProviderOwnerType = "preset"
)

type ProviderOwnerReference struct {
	Type                 ProviderOwnerType             `json:"type" jsonschema:"required,description=Provider owner class,enum=core,enum=custom,enum=plugin,enum=preset"`
	Construction         providerregistry.Construction `json:"construction" jsonschema:"required,description=Exact host construction selected for this provider owner"`
	CompatibilityAdapter providerregistry.Construction `json:"compatibility_adapter,omitempty" jsonschema:"description=Exact compatibility adapter delegated by a plugin owner"`
}

type ProviderPluginReference struct {
	ID      string `json:"id" jsonschema:"required,description=Stable provider-plugin identity"`
	Version string `json:"version,omitempty" jsonschema:"description=Plugin version that last owned this configuration"`
}

type ProviderPresetReference struct {
	ID      string `json:"id" jsonschema:"required,description=Stable provider-preset identity"`
	Version string `json:"version,omitempty" jsonschema:"description=Preset version that last supplied this provider catalog"`
	Digest  string `json:"digest,omitempty" jsonschema:"description=Canonical digest of the provider-preset bundle"`
}

type ProviderAPIKeyCredential struct {
	Owner  providerregistry.RegistrationOwner
	APIKey string
}

type ProviderOAuthCredential struct {
	Owner providerregistry.RegistrationOwner
	Token *oauth.Token
}

func ProviderCredentialOwner(providerID string, credential any) (providerregistry.RegistrationOwner, error) {
	switch value := credential.(type) {
	case string:
		return providerregistry.RegistrationOwner{}, fmt.Errorf("API key for provider %s is missing its initiating owner", providerID)
	case ProviderAPIKeyCredential:
		if value.Owner.ProviderID != providerID {
			return providerregistry.RegistrationOwner{}, fmt.Errorf("API key owner provider %s does not match credential provider %s", value.Owner.ProviderID, providerID)
		}
		return value.Owner, nil
	case ProviderOAuthCredential:
		if value.Token == nil {
			return providerregistry.RegistrationOwner{}, fmt.Errorf("OAuth token is nil")
		}
		if value.Owner.ProviderID != providerID {
			return providerregistry.RegistrationOwner{}, fmt.Errorf("OAuth owner provider %s does not match credential provider %s", value.Owner.ProviderID, providerID)
		}
		return value.Owner, nil
	case *oauth.Token:
		return providerregistry.RegistrationOwner{}, fmt.Errorf("OAuth token for provider %s is missing its initiating owner", providerID)
	default:
		return providerregistry.RegistrationOwner{}, fmt.Errorf("unsupported API key type %T", credential)
	}
}

type ProviderConfig struct {
	// The provider's id.
	ID string `json:"id,omitempty" jsonschema:"description=Unique identifier for the provider,example=openai"`
	// The provider's name, used for display purposes.
	Name string `json:"name,omitempty" jsonschema:"description=Human-readable name for the provider,example=OpenAI"`
	// The provider's API endpoint.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Base URL for the provider's API,format=uri,example=https://api.openai.com/v1"`
	// The provider type. Empty custom-provider types default to openai-compat;
	// registered local aliases use the same protocol.
	Type catalog.Type `json:"type,omitempty" jsonschema:"description=OpenAI-compatible provider type,default=openai-compat"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
	// The original API key template before resolution (for re-resolution on auth errors).
	APIKeyTemplate string `json:"-"`
	// OAuthToken for providers that use OAuth2 authentication.
	OAuthToken *oauth.Token `json:"oauth,omitempty" jsonschema:"description=OAuth2 token for authentication with the provider"`
	// Plugin records durable ownership so configuration and selections remain
	// unavailable rather than falling through to a generic provider when the
	// bundle is missing, disabled, invalid, incompatible, or untrusted.
	Owner  *ProviderOwnerReference  `json:"owner,omitempty" jsonschema:"description=Exact provider ownership and construction reference"`
	Plugin *ProviderPluginReference `json:"plugin,omitempty" jsonschema:"description=Provider-plugin ownership reference"`
	Preset *ProviderPresetReference `json:"preset,omitempty" jsonschema:"description=Provider-preset catalog ownership reference"`
	// Marks the provider as disabled.
	Disable bool `json:"disable,omitempty" jsonschema:"description=Whether this provider is disabled,default=false"`

	// Custom system prompt prefix.
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty" jsonschema:"description=Custom prefix to add to system prompts for this provider"`

	// Tooling instruction profile used for this provider.
	ToolingInstructions string `json:"tooling_instructions,omitempty" jsonschema:"description=Tooling instruction profile for this provider,enum=crux,enum=native,default=crux"`

	// Extra headers to send with each request to the provider. Values
	// run through shell expansion at config-load time, so $VAR and
	// $(cmd) work the same way they do in MCP headers. A header whose
	// value resolves to the empty string (unset bare $VAR under
	// lenient nounset, $(echo), or literal "") is omitted from the
	// outgoing request rather than sent as "Header:".
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" jsonschema:"description=Additional HTTP headers to send with requests"`
	// ExtraBody is merged verbatim into OpenAI-compatible request
	// bodies. String values are NOT shell-expanded: this is a plain
	// JSON passthrough so that arbitrary provider-extension fields
	// (numbers, nested objects, booleans) round-trip without a
	// recursive walker guessing at intent. If you need an env-var-
	// driven value at request time, put it in extra_headers, or in
	// the provider's top-level api_key / base_url, all of which do
	// expand.
	ExtraBody map[string]any `json:"extra_body,omitempty" jsonschema:"description=Additional fields to include in request bodies\\, only works with openai-compatible providers"`

	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for this provider"`

	// Configuration preserves declarative plugin configuration values. Values
	// are validated against the active provider registry when that plugin is
	// installed; missing-plugin values remain lossless and untouched.
	Configuration map[string]any `json:"configuration,omitempty" jsonschema:"description=Declarative provider-plugin configuration values"`

	// Used to pass extra parameters to the provider.
	ExtraParams map[string]string `json:"-"`

	// Skip cost accumulation for this provider when using subscription or flat rate billing.
	FlatRate bool `json:"flat_rate,omitempty" jsonschema:"description=Flat-rate mode for this provider"`

	// AutoDiscoverModels controls model discovery via /v1/models endpoint.
	// When Models is empty and this is nil or true, Crux auto-discovers
	// models. When true and Models is non-empty, discovered models are
	// merged in (user-specified models take precedence). When false,
	// only explicitly listed models are used.
	AutoDiscoverModels *bool `json:"discover_models,omitempty" jsonschema:"description=Auto-discover models from /v1/models endpoint. When true with existing models they are merged (yours win),default=true"`

	// The provider models
	Models []catalog.Model `json:"models,omitempty" jsonschema:"description=List of models available from this provider"`
}

// ToProvider converts the [ProviderConfig] to a [catalog.Provider].
func (c *ProviderConfig) ToProvider() catalog.Provider {
	// Convert config provider to provider.Provider format
	provider := catalog.Provider{
		Name:   c.Name,
		ID:     catalog.ProviderID(c.ID),
		Models: make([]catalog.Model, len(c.Models)),
	}

	// Convert models
	for i, model := range c.Models {
		provider.Models[i] = catalog.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
		}
	}

	return provider
}

func (c *ProviderConfig) SetupGitHubCopilot() {
	maps.Copy(c.ExtraHeaders, copilot.Headers())
}

type MCPType string

const (
	MCPStdio MCPType = "stdio"
	MCPSSE   MCPType = "sse"
	MCPHttp  MCPType = "http"
)

type MCPConfig struct {
	Command       string            `json:"command,omitempty" jsonschema:"description=Command to execute for stdio MCP servers,example=npx"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set for the MCP server"`
	Args          []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the MCP server command"`
	Type          MCPType           `json:"type" jsonschema:"required,description=Type of MCP connection,enum=stdio,enum=sse,enum=http,default=stdio"`
	URL           string            `json:"url,omitempty" jsonschema:"description=URL for HTTP or SSE MCP servers,format=uri,example=http://localhost:3000/mcp"`
	Disabled      bool              `json:"disabled,omitempty" jsonschema:"description=Whether this MCP server is disabled,default=false"`
	DisabledTools []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to disable,example=get-library-doc"`
	EnabledTools  []string          `json:"enabled_tools,omitempty" jsonschema:"description=Allow list of tools from this MCP server,example=get-library-doc"`
	Timeout       int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for MCP server connections,default=10,example=30,example=60,example=120"`

	// Headers are HTTP headers for HTTP/SSE MCP servers. Values run
	// through shell expansion at MCP startup, so $VAR and $(cmd)
	// work. A header whose value resolves to the empty string (unset
	// bare $VAR under lenient nounset, $(echo), or literal "") is
	// omitted from the outgoing request rather than sent as
	// "Header:".
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=HTTP headers for HTTP/SSE MCP servers"`

	// OAuth enables the MCP OAuth 2.1 authorization flow for HTTP
	// transport servers. When true, the client uses dynamic client
	// registration and opens a browser for the user to authorize.
	// Tokens are persisted automatically. Only supported for type=http.
	OAuth bool `json:"oauth,omitempty" jsonschema:"description=Enable OAuth 2.1 authorization flow for this MCP server (HTTP transport only),default=false"`

	// OAuthClientID is an optional pre-registered OAuth client ID. Set
	// it for servers that do not support dynamic client registration
	// (e.g. GitHub, Slack) and instead issue client credentials when you
	// register an OAuth app. Values run through shell expansion, so
	// $VAR and $(cmd) work.
	OAuthClientID string `json:"oauth_client_id,omitempty" jsonschema:"description=Pre-registered OAuth client ID for servers without dynamic client registration"`

	// OAuthClientSecret is the optional secret paired with
	// OAuthClientID for confidential clients. Values run through shell
	// expansion, so $VAR and $(cmd) work.
	OAuthClientSecret string `json:"oauth_client_secret,omitempty" jsonschema:"description=Pre-registered OAuth client secret paired with oauth_client_id"`

	// OAuthCallbackPort pins the localhost port used for the OAuth
	// redirect listener. Set this when the OAuth provider requires an
	// exact-match callback URL (e.g. GitHub OAuth Apps). When omitted,
	// Crux picks the first free port from its default range.
	OAuthCallbackPort int `json:"oauth_callback_port,omitempty" jsonschema:"description=Fixed localhost port for the OAuth callback, required by providers that enforce exact-match redirect URIs"`

	// OAuthToken is the persisted OAuth token for this server. It is
	// managed internally and stored in the global data config.
	OAuthToken *oauth.Token `json:"oauth_token,omitempty" jsonschema:"-"`
}

// isOrphanedToken reports whether this entry is a leftover OAuth token
// with no real server config.
func (m MCPConfig) isOrphanedToken() bool {
	return m.Type == "" && m.Command == "" && m.URL == "" && m.OAuthToken != nil
}

type LSPConfig struct {
	Disabled    bool              `json:"disabled,omitempty" jsonschema:"description=Whether this LSP server is disabled,default=false"`
	Command     string            `json:"command,omitempty" jsonschema:"description=Command to execute for the LSP server,example=gopls"`
	Args        []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the LSP server command"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set to the LSP server command"`
	FileTypes   []string          `json:"filetypes,omitempty" jsonschema:"description=File types this LSP server handles,example=go,example=mod,example=rs,example=c,example=js,example=ts"`
	RootMarkers []string          `json:"root_markers,omitempty" jsonschema:"description=Files or directories that indicate the project root,example=go.mod,example=package.json,example=Cargo.toml"`
	InitOptions map[string]any    `json:"init_options,omitempty" jsonschema:"description=Initialization options passed to the LSP server during initialize request"`
	Options     map[string]any    `json:"options,omitempty" jsonschema:"description=LSP server-specific settings passed during initialization"`
	Timeout     int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for LSP server initialization,default=30,example=60,example=120"`
}

type TUIOptions struct {
	CompactMode bool   `json:"compact_mode,omitempty" jsonschema:"description=Enable compact mode for the TUI interface,default=false"`
	DiffMode    string `json:"diff_mode,omitempty" jsonschema:"description=Diff mode for the TUI interface,enum=unified,enum=split"`
	// Here we can add themes later or any TUI related options
	//

	Completions Completions `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool       `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
	Scrollbar   string      `json:"scrollbar,omitempty" jsonschema:"description=Chat scrollbar visibility,enum=default,enum=always,enum=never,default=default"`
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

func (c Completions) Limits() (depth, items int) {
	return ptrValOr(c.MaxDepth, 0), ptrValOr(c.MaxItems, 0)
}

// Scrollbar visibility options.
const (
	ScrollbarDefault = "default" // Auto-hide after 2 seconds
	ScrollbarAlways  = "always"  // Always show when content exceeds viewport
	ScrollbarNever   = "never"   // Never show scrollbar
)

type Permissions struct {
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=view"`
}

type Options struct {
	ContextPaths         []string    `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI,example=.cursorrules,example=CRUX.md"`
	GlobalContextPaths   []string    `json:"global_context_paths,omitempty" jsonschema:"description=Paths to files containing global context information for the AI,default=~/.ai-cli/crux/CRUX.md,default=~/.ai-cli/AGENTS.md"`
	SkillsPaths          []string    `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.ai-cli/crux/skills,example=./skills"`
	TUI                  *TUIOptions `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	Debug                bool        `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DebugLSP             bool        `json:"debug_lsp,omitempty" jsonschema:"description=Enable debug logging for LSP servers,default=false"`
	DisableAutoSummarize bool        `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	// DataDirectory is where Crux keeps per-project state such as
	// the SQLite database and workspace overrides. Relative paths are
	// resolved against the working directory; absolute paths are used
	// verbatim. After defaulting the stored value is always absolute.
	DataDirectory           string   `json:"data_directory,omitempty" jsonschema:"description=Directory for storing application data. Relative paths are resolved against the working directory; absolute paths are used as-is.,default=.crux,example=.crux"`
	DisabledTools           []string `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=sourcegraph"`
	DisableDefaultProviders bool     `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore core and installed provider catalogs. When enabled\\, providers must be fully specified in the config file with base_url\\, models\\, and api_key - no merging with defaults occurs,default=false"`
	InitializeAs            string   `json:"initialize_as,omitempty" jsonschema:"description=Context file to create or update during project initialization. Defaults to the per-project ~/.ai-cli/project-prompts path.,example=AGENTS.md,example=CRUX.md,example=CLAUDE.md,example=docs/LLMs.md"`
	AutoLSP                 *bool    `json:"auto_lsp,omitempty" jsonschema:"description=Automatically setup LSPs based on root markers,default=true"`
	Progress                *bool    `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	Notifications           string   `json:"notifications,omitempty" jsonschema:"description=Notification style to use. Options: auto (default)\\, native\\, osc\\, bell\\, disabled. Auto selects based on environment: native for local sessions\\, osc for SSH (with automatic OSC 99/777 detection).,enum=auto,enum=native,enum=osc,enum=bell,enum=disabled,default=auto"`
	DisabledSkills          []string `json:"disabled_skills,omitempty" jsonschema:"description=List of skill names to disable and hide from the agent,example=crux-config"`

	// InstructionMode controls which optional instruction sources are active:
	//   "all"     - tooling and project context (default)
	//   "project" - project context without tooling
	//   "native"  - tooling without project context
	// Dynamic runtime, memory, MCP, and provider context remain independent.
	InstructionMode   string `json:"instruction_mode,omitempty" jsonschema:"description=Which optional instruction sources are active: all (default)\\, project without tooling\\, or native for tooling without project context. Dynamic runtime\\, memory\\, MCP\\, and provider context remain active.,enum=all,enum=project,enum=native,default=all"`
	ResponseVerbosity string `json:"response_verbosity,omitempty" jsonschema:"description=GPT-5.6 final response verbosity sent through the ChatGPT Codex wire adapter,enum=low,enum=medium,enum=high"`
	AnalysisEffort    string `json:"analysis_effort,omitempty" jsonschema:"description=GPT-5.6 reasoning effort sent through the ChatGPT Codex wire adapter,enum=none,enum=low,enum=medium,enum=high,enum=xhigh,enum=max"`

	// DisabledInstructionSections lists Crux tooling instruction section IDs to
	// skip when building the system prompt. Section IDs match the file
	// names in internal/agent/templates/sections/ without the .md extension.
	DisabledInstructionSections []string `json:"disabled_instruction_sections,omitempty" jsonschema:"description=List of Crux tooling instruction section IDs to disable,example=whitespace,example=memory"`
}

func (o *Options) validatePromptOptions() error {
	if o.ResponseVerbosity != "" && !slices.Contains([]string{"low", "medium", "high"}, o.ResponseVerbosity) {
		return fmt.Errorf("response_verbosity must be low, medium, or high")
	}
	if o.AnalysisEffort != "" && !slices.Contains([]string{"none", "low", "medium", "high", "xhigh", "max"}, o.AnalysisEffort) {
		return fmt.Errorf("analysis_effort must be none, low, medium, high, xhigh, or max")
	}
	return nil
}

type MCPs map[string]MCPConfig

type MCP struct {
	Name string    `json:"name"`
	MCP  MCPConfig `json:"mcp"`
}

func (m MCPs) Sorted() []MCP {
	sorted := make([]MCP, 0, len(m))
	for k, v := range m {
		sorted = append(sorted, MCP{
			Name: k,
			MCP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b MCP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

type LSPs map[string]LSPConfig

type LSP struct {
	Name string    `json:"name"`
	LSP  LSPConfig `json:"lsp"`
}

func (l LSPs) Sorted() []LSP {
	sorted := make([]LSP, 0, len(l))
	for k, v := range l {
		sorted = append(sorted, LSP{
			Name: k,
			LSP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b LSP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

// ResolvedEnv returns m.Env with every value expanded through the
// given resolver. The returned slice is of the form "KEY=value" sorted
// by key so callers get deterministic output; the receiver's Env map is
// not mutated. On the first resolution failure it returns nil and an
// error that identifies the offending key; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Callers are expected to surface it
// (for MCP, via StateError on the status card) rather than silently
// spawn the server with an empty credential.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim and expansion happens on the server.
func (m MCPConfig) ResolvedEnv(r VariableResolver) ([]string, error) {
	return resolveEnvs(m.Env, r)
}

// ResolvedArgs returns m.Args with every element expanded through the
// given resolver. A fresh slice is allocated; m.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(m.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(m.Args))
	for i, a := range m.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedURL returns m.URL expanded through the given resolver. The
// receiver is not mutated. Errors from the resolver are already
// sanitized by ResolveValue and are wrapped with %w for errors.Is/As.
//
// URLs run through the same shell-expansion pipeline as the other
// fields, so a literal '$' (e.g. OData query strings containing
// $filter/$select) must be escaped as '\$' or '${DOLLAR:-$}' to avoid
// being interpreted as a variable reference. Same constraint already
// applies to command, args, env, and headers.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedURL(r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := r.ResolveValue(m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// ResolvedHeaders returns m.Headers with every value expanded through
// the given resolver. A fresh map is allocated; m.Headers is never
// mutated. On the first resolution failure it returns nil and an error
// identifying the offending header name; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// A header whose value resolves to the empty string (unset bare $VAR
// under lenient nounset, $(echo), or literal "") is omitted from the
// returned map — sending "X-Auth:" with an empty value is rejected by
// some providers and the user's intent in "optional, env-gated
// header" is clearly "absent when the var isn't set."
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedHeaders(r VariableResolver) (map[string]string, error) {
	if len(m.Headers) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m.Headers))
	// Sort keys so failures are reported deterministically when more
	// than one header would fail.
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(m.Headers[k])
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// ResolvedArgs returns l.Args with every element expanded through the
// given resolver. A fresh slice is allocated; l.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// Empty resolved values are kept (a deliberate "empty positional arg"
// like --flag "" is sometimes valid), matching MCPConfig.ResolvedArgs.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim.
func (l LSPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(l.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(l.Args))
	for i, a := range l.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedEnv returns l.Env with every value expanded through the
// given resolver. A fresh map is allocated; l.Env is never mutated.
// On the first resolution failure it returns nil and an error that
// identifies the offending key; the inner resolver error is already
// sanitized by ResolveValue and is wrapped with %w so errors.Is/As
// continues to work.
//
// Empty resolved values are kept ("FOO=" is a legitimate request;
// opt out via ${VAR:+...}), matching MCPConfig.ResolvedEnv.
//
// Shape note: this returns map[string]string rather than the []string
// shape MCPConfig.ResolvedEnv uses because the consumer
// (powernap.ClientConfig.Environment in internal/lsp/client.go) takes
// a map directly — returning a []string here would only force a
// round-trip back to a map at the call site.
//
// See ResolvedArgs for guidance on picking a resolver.
func (l LSPConfig) ResolvedEnv(r VariableResolver) (map[string]string, error) {
	if len(l.Env) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(l.Env))
	// Sort keys so failures are reported deterministically when more
	// than one value would fail.
	keys := make([]string, 0, len(l.Env))
	for k := range l.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(l.Env[k])
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

type AgentScriptVariable struct {
	Flag     string
	Required bool
	Default  *string
	Value    *string
	Values   []string
}

type AgentScript struct {
	Path      string
	Timeout   time.Duration
	Variables map[string]AgentScriptVariable
}

type Agent struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// This is the id of the system prompt used by the agent
	Disabled bool `json:"disabled,omitempty"`

	Model                SelectedModelType `json:"model" jsonschema:"required,description=The model type to use for this agent,enum=large,enum=small,default=large"`
	PrimaryModelOverride *SelectedModel    `json:"-" jsonschema:"-"`
	Instructions         string            `json:"-" jsonschema:"-"`
	DefinitionPath       string            `json:"-" jsonschema:"-"`
	AllowAllTools        bool              `json:"-" jsonschema:"-"`
	Script               *AgentScript      `json:"-" jsonschema:"-"`

	// The available tools for the agent
	//  if this is nil, all tools are available
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// this tells us which MCPs are available for this agent
	//  if this is empty all mcps are available
	//  the string array is the list of tools from the AllowedMCP the agent has available
	//  if the string array is nil, all tools from the AllowedMCP are available
	AllowedMCP map[string][]string `json:"allowed_mcp,omitempty"`

	// Overrides the context paths for this agent
	ContextPaths []string `json:"context_paths,omitempty"`
}

type Tools struct {
	Ls             ToolLs             `json:"ls,omitzero"`
	Search         ToolSearch         `json:"search,omitzero"`
	CodebaseSearch ToolCodebaseSearch `json:"codebase_search,omitzero"`
}

type ToolLs struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

// Limits returns the user-defined max-depth and max-items, or their defaults.
func (t ToolLs) Limits() (depth, items int) {
	return ptrValOr(t.MaxDepth, 0), ptrValOr(t.MaxItems, 0)
}

type ToolSearch struct {
	FilesTimeout   *time.Duration `json:"files_timeout,omitempty" jsonschema:"description=Timeout for file path search calls,default=30s,example=10s"`
	ContentTimeout *time.Duration `json:"content_timeout,omitempty" jsonschema:"description=Timeout for file content search calls,default=5s,example=10s"`
}

func (t ToolSearch) GetFilesTimeout() time.Duration {
	return ptrValOr(t.FilesTimeout, 30*time.Second)
}

func (t ToolSearch) GetContentTimeout() time.Duration {
	return ptrValOr(t.ContentTimeout, 5*time.Second)
}

type ToolCodebaseSearch struct {
	DatabasePath   string   `json:"database_path,omitempty" jsonschema:"description=Path to a tui-files codebase index database or directory containing project databases"`
	StoreDirectory string   `json:"store_directory,omitempty" jsonschema:"description=Directory containing standalone partitioned codebase search stores"`
	ANNDirectory   string   `json:"ann_directory,omitempty" jsonschema:"description=Deprecated alias for store_directory"`
	Enabled        *bool    `json:"enabled,omitempty" jsonschema:"description=Whether background semantic indexing and codebase search are enabled,default=false"`
	IncludePaths   []string `json:"include_paths,omitempty" jsonschema:"description=Project-relative path prefixes to include in the semantic index"`
	ExcludePaths   []string `json:"exclude_paths,omitempty" jsonschema:"description=Project-relative path prefixes to exclude from the semantic index"`
}

func (t ToolCodebaseSearch) GetStoreDirectory() string {
	if t.StoreDirectory != "" {
		return t.StoreDirectory
	}
	return t.ANNDirectory
}

func (t ToolCodebaseSearch) IsEnabled() bool {
	return t.Enabled != nil && *t.Enabled
}

// HookConfig defines a user-configured shell command that fires on a hook
// event (e.g. PreToolUse). This is a pure-data struct: matcher compilation
// is owned by hooks.Runner so a JSON round-trip, merge, or reload can't
// silently drop compiled state.
type HookConfig struct {
	// Friendly display name shown in the TUI. Falls back to Command when empty.
	Name string `json:"name,omitempty" jsonschema:"description=Friendly display name shown in the TUI for this hook"`
	// Regex pattern tested against the tool name. Empty means match all.
	Matcher string `json:"matcher,omitempty" jsonschema:"description=Regex pattern tested against the tool name. Empty means match all tools."`
	// Shell command to execute.
	Command string `json:"command" jsonschema:"required,description=Shell command to execute when the hook fires"`
	// Timeout in seconds. Default 30.
	Timeout int `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for the hook command,default=30"`
}

// DisplayName returns the hook name for display purposes. It returns Name
// when set, otherwise falls back to Command.
func (h *HookConfig) DisplayName() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Command
}

// TimeoutDuration returns the hook timeout as a time.Duration, defaulting
// to 30s.
func (h *HookConfig) TimeoutDuration() time.Duration {
	if h.Timeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(h.Timeout) * time.Second
}

// Config holds the configuration for crux.
type Config struct {
	Schema string `json:"$schema,omitempty"`

	// We currently only support large/small as values here.
	Models map[SelectedModelType]SelectedModel `json:"models,omitempty" jsonschema:"description=Model configurations for different model types,example={\"large\":{\"model\":\"gpt-4o\",\"provider\":\"openai\"}}"`

	// Recently used models stored in the data directory config.
	RecentModels map[SelectedModelType][]SelectedModel `json:"recent_models,omitempty" jsonschema:"-"`

	// The providers that are configured
	Providers *csync.Map[string, ProviderConfig] `json:"providers,omitempty" jsonschema:"description=AI provider configurations"`

	Images *ImageConfiguration `json:"images,omitempty" jsonschema:"description=Installed image provider selection and execution-host configuration"`

	MCP MCPs `json:"mcp,omitempty" jsonschema:"description=Model Context Protocol server configurations"`

	LSP LSPs `json:"lsp,omitempty" jsonschema:"description=Language Server Protocol configurations"`

	Options *Options `json:"options,omitempty" jsonschema:"description=General application options"`

	Permissions *Permissions `json:"permissions,omitempty" jsonschema:"description=Permission settings for tool usage"`

	Tools Tools `json:"tools,omitzero" jsonschema:"description=Tool configurations"`

	Hooks map[string][]HookConfig `json:"hooks,omitempty" jsonschema:"description=User-defined shell commands that fire on hook events (e.g. PreToolUse)"`

	// Env is a map of environment variables set on startup.
	Env map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set on startup"`

	Agents map[string]Agent `json:"-"`

	providerScan            *ProviderScan
	transportProviderOwners map[string]providerregistry.RegistrationOwner
	explicitModels          map[SelectedModelType]bool
}

// cloneForWrite returns a copy of c that the store's typed field mutators
// may modify without racing readers of the currently published Config.
//
// Reads of a published Config take no lock beyond the pointer load, so a
// mutator must never write through the live pointer. Instead it clones,
// mutates the clone, and atomically swaps it in. The clone gives fresh
// copies of every field a typed mutator touches in place — Models,
// RecentModels, Providers, MCP, and Options (with its nested TUI pointer).
// The remaining fields are immutable after load from the mutators' standpoint
// and are shared.
func (c *Config) cloneForWrite() *Config {
	nc := *c
	nc.Images = cloneImageConfiguration(c.Images)
	nc.Models = maps.Clone(c.Models)
	nc.transportProviderOwners = maps.Clone(c.transportProviderOwners)
	nc.explicitModels = maps.Clone(c.explicitModels)
	nc.RecentModels = maps.Clone(c.RecentModels)
	if c.Providers != nil {
		providers := make(map[string]ProviderConfig, c.Providers.Len())
		for id, provider := range c.Providers.Seq2() {
			providers[id] = cloneProviderConfig(provider)
		}
		nc.Providers = csync.NewMapFrom(providers)
	}
	nc.MCP = maps.Clone(c.MCP)
	if c.Options != nil {
		opts := *c.Options
		if c.Options.TUI != nil {
			tui := *c.Options.TUI
			opts.TUI = &tui
		}
		nc.Options = &opts
	}
	return &nc
}

func cloneProviderConfig(provider ProviderConfig) ProviderConfig {
	provider.OAuthToken = cloneOAuthToken(provider.OAuthToken)
	provider.Owner = clonePointer(provider.Owner)
	provider.Plugin = clonePointer(provider.Plugin)
	provider.Preset = clonePointer(provider.Preset)
	provider.ExtraHeaders = maps.Clone(provider.ExtraHeaders)
	provider.ExtraBody = cloneProviderOptions(provider.ExtraBody)
	provider.ProviderOptions = cloneProviderOptions(provider.ProviderOptions)
	provider.Configuration = cloneProviderOptions(provider.Configuration)
	provider.ExtraParams = maps.Clone(provider.ExtraParams)
	provider.AutoDiscoverModels = clonePointer(provider.AutoDiscoverModels)
	provider.Models = cloneProvider(catalog.Provider{Models: provider.Models}).Models
	return provider
}

func cloneOAuthToken(token *oauth.Token) *oauth.Token {
	if token == nil {
		return nil
	}
	clone := *token
	clone.Client = clonePointer(token.Client)
	return &clone
}

func (c *Config) captureExplicitModels() {
	c.explicitModels = make(map[SelectedModelType]bool, len(c.Models))
	for modelType := range c.Models {
		c.explicitModels[modelType] = true
	}
}

func (c *Config) markModelExplicit(modelType SelectedModelType) {
	if c.explicitModels == nil {
		c.explicitModels = make(map[SelectedModelType]bool)
	}
	c.explicitModels[modelType] = true
}

func (c *Config) modelExplicit(modelType SelectedModelType) bool {
	if c.explicitModels == nil {
		_, ok := c.Models[modelType]
		return ok
	}
	return c.explicitModels[modelType]
}

// ensureTUI returns c.Options.TUI, allocating Options and TUI as needed so
// callers can assign TUI fields without nil checks.
func (c *Config) ensureTUI() *TUIOptions {
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	return c.Options.TUI
}

// EnabledProviders returns only providers this process can actually construct.
// Retained plugin-owned providers remain in Config.Providers when unavailable,
// but must not be reclassified as enabled generic providers or chosen as new
// defaults merely because their durable configuration still exists.
func (c *Config) EnabledProviders() []ProviderConfig {
	var enabled []ProviderConfig
	for id, p := range c.Providers.Seq2() {
		if !p.Disable && c.IsProviderAvailable(id) {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

// IsConfigured returns true if at least one provider is configured and
// available. It is a broad process-readiness check used before selected models
// have necessarily been resolved. It does not authorize replacing a retained
// unavailable plugin selection with whichever provider made this return true.
func (c *Config) IsConfigured() bool {
	return len(c.EnabledProviders()) > 0
}

// CanInitializeAgent returns true only when both selected models can be
// constructed by this host generation. It is deliberately separate from
// IsConfigured: another available provider must not make an unavailable
// plugin-backed selection appear constructible. Callers must surface that
// selected integration's unavailability rather than silently changing models.
func (c *Config) CanInitializeAgent() bool {
	large, largeOK := c.Models[SelectedModelTypeLarge]
	small, smallOK := c.Models[SelectedModelTypeSmall]
	return largeOK && smallOK &&
		c.IsModelAvailable(large.Provider, large.Model) &&
		c.IsModelAvailable(small.Provider, small.Model)
}

// RedactedForTransport returns a snapshot safe to send to a frontend. Provider
// credentials and manifest fields marked secret are removed while selections,
// catalogs, and non-secret presentation configuration remain available. Plugin
// configuration is private by default: do not expose secret manifest fields or
// OAuth values merely to diagnose registration or retained-selection behavior.
func (c *Config) RedactedForTransport() *Config {
	if c == nil {
		return nil
	}
	result := *c
	result.Images = cloneImageConfiguration(c.Images)
	if result.Images != nil {
		for backend, provider := range result.Images.Providers {
			provider.Configuration = nil
			provider.Credentials = nil
			provider.BrowserProfiles = nil
			result.Images.Providers[backend] = provider
		}
	}
	providers := csync.NewMap[string, ProviderConfig]()
	if c.Providers != nil {
		for id, provider := range c.Providers.Seq2() {
			provider.APIKey = ""
			provider.APIKeyTemplate = ""
			provider.OAuthToken = nil
			provider.ExtraHeaders = nil
			provider.Configuration = maps.Clone(provider.Configuration)
			registration, registered := c.ProviderRegistration(id)
			if provider.Preset != nil {
				provider.BaseURL = ""
				provider.SystemPromptPrefix = ""
				provider.ExtraBody = nil
				provider.ProviderOptions = nil
				provider.Configuration = nil
				provider.ExtraParams = nil
			} else if provider.Plugin != nil && (!registered || registration.Manifest == nil) {
				provider.Configuration = nil
			} else if registered && registration.Manifest != nil {
				for field, display := range registration.Manifest.Configuration.Fields {
					if display.Secret {
						delete(provider.Configuration, field)
					}
				}
			}
			providers.Set(id, provider)
		}
	}
	result.Providers = providers
	return &result
}

// GetModel resolves a model only inside its configured provider catalog. Do not
// make this search other providers: a matching model ID elsewhere is not an
// equivalent selection and must never enable cross-provider substitution.
func (c *Config) GetModel(provider, model string) *catalog.Model {
	if providerConfig, ok := c.Providers.Get(provider); ok {
		for _, m := range providerConfig.Models {
			if m.ID == model {
				return &m
			}
		}
	}
	return nil
}

func providerOwnerReferenceForRegistration(registration providerregistry.Registration) *ProviderOwnerReference {
	ownerType := ProviderOwnerCore
	if registration.Manifest != nil {
		ownerType = ProviderOwnerPlugin
	}
	return &ProviderOwnerReference{
		Type:                 ownerType,
		Construction:         registration.Construction,
		CompatibilityAdapter: registration.CompatibilityAdapter,
	}
}

func providerPresetOwnerReference() *ProviderOwnerReference {
	return &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}
}

func pluginReferenceMatches(reference *ProviderPluginReference, registration providerregistry.Registration) bool {
	if reference == nil || registration.Manifest == nil || registration.Manifest.ID != reference.ID {
		return false
	}
	return reference.Version != "" && registration.Manifest.Version == reference.Version
}

func presetReferenceMatches(configured *ProviderPresetReference, active ProviderPresetReference) bool {
	if configured == nil || configured.ID != active.ID {
		return false
	}
	return configured.Version != "" && configured.Version == active.Version &&
		configured.Digest != "" && configured.Digest == active.Digest
}

func providerPresetReferenceMatches(providerID string, configured *ProviderPresetReference, active ProviderPresetReference) bool {
	if _, _, migrated := providerplugin.MigratedProviderPreset(providerID); migrated {
		return configured != nil && configured.Digest != "" && configured.Digest == active.Digest &&
			providerplugin.IsCanonicalMigratedProviderPreset(providerID, configured.ID, configured.Version) &&
			providerplugin.IsCanonicalMigratedProviderPresetBundle(providerID, active.ID, active.Version, active.Digest)
	}
	return presetReferenceMatches(configured, active)
}

func providerRegistrationForProvider(registry *providerregistry.Registry, providerID string, provider ProviderConfig) (providerregistry.Registration, bool) {
	if registry == nil || providerID == "" || provider.ID != "" && provider.ID != providerID ||
		provider.Owner == nil || provider.Owner.Type == "" || provider.Owner.Construction == "" || provider.Preset != nil {
		return providerregistry.Registration{}, false
	}
	checked := provider
	owner := *provider.Owner
	checked.Owner = &owner
	if err := validateConfiguredProviderOwner(providerID, checked); err != nil {
		return providerregistry.Registration{}, false
	}
	registration, ok := registry.Lookup(providerID)
	if !ok || registration.ProviderID != providerID {
		return providerregistry.Registration{}, false
	}
	switch owner.Type {
	case ProviderOwnerPlugin:
		if provider.Plugin == nil || !pluginReferenceMatches(provider.Plugin, registration) ||
			registration.Manifest == nil || owner.Construction != registration.Construction ||
			owner.CompatibilityAdapter != registration.CompatibilityAdapter {
			return providerregistry.Registration{}, false
		}
	case ProviderOwnerCore:
		if provider.Plugin != nil || registration.Manifest != nil ||
			owner.Construction != registration.Construction || owner.CompatibilityAdapter != "" {
			return providerregistry.Registration{}, false
		}
	default:
		return providerregistry.Registration{}, false
	}
	if registration.Manifest != nil {
		bound, err := providerregistry.BindRegistrationConfiguration(registration, provider.Configuration)
		if err != nil {
			return providerregistry.Registration{}, false
		}
		registration = bound
	}
	return registration, true
}

func (c *Config) providerRegistration(registry *providerregistry.Registry, providerID string) (providerregistry.Registration, bool) {
	if c == nil || registry == nil {
		return providerregistry.Registration{}, false
	}
	provider, configured := ProviderConfig{}, false
	if c.Providers != nil {
		provider, configured = c.Providers.Get(providerID)
	}
	if configured && provider.Owner != nil {
		return providerRegistrationForProvider(registry, providerID, provider)
	}
	if configured && provider.Preset != nil {
		return providerregistry.Registration{}, false
	}
	registration, ok := registry.Lookup(providerID)
	if !ok || registration.ProviderID != providerID {
		return providerregistry.Registration{}, false
	}
	if configured {
		if provider.Plugin != nil {
			if !pluginReferenceMatches(provider.Plugin, registration) {
				return providerregistry.Registration{}, false
			}
		} else {
			construction, core := coreProviderConstruction(providerID)
			if !core || registration.Manifest != nil || registration.Construction != construction || registration.CompatibilityAdapter != "" {
				return providerregistry.Registration{}, false
			}
		}
	}
	if configured && registration.Manifest != nil {
		bound, err := providerregistry.BindRegistrationConfiguration(registration, provider.Configuration)
		if err != nil {
			return providerregistry.Registration{}, false
		}
		registration = bound
	}
	return registration, true
}

func (c *Config) ProviderRegistration(providerID string) (providerregistry.Registration, bool) {
	return c.providerRegistration(c.providerCapabilities(), providerID)
}

func (c *Config) ProviderOwner(providerID string) (providerregistry.RegistrationOwner, bool) {
	if c != nil && c.providerCapabilities() == nil && c.transportProviderOwners != nil {
		owner, ok := c.transportProviderOwners[providerID]
		return owner, ok
	}
	return providerOwnerForConfig(c, c.providerCapabilities(), providerID)
}

func (c *Config) BindProviderSurfaceOwners(surfaces []providerregistry.Surface) error {
	if c == nil {
		return fmt.Errorf("cannot bind provider owners to a nil configuration")
	}
	owners := make(map[string]providerregistry.RegistrationOwner)
	for _, surface := range surfaces {
		if surface.Owner == nil {
			continue
		}
		if surface.ID == "" || surface.Owner.ProviderID != surface.ID {
			return fmt.Errorf("provider surface %q has mismatched owner %q", surface.ID, surface.Owner.ProviderID)
		}
		if _, exists := owners[surface.ID]; exists {
			return fmt.Errorf("provider surface %q has duplicate owners", surface.ID)
		}
		owners[surface.ID] = *surface.Owner
	}
	c.transportProviderOwners = owners
	return nil
}

func (c *Config) ProviderRegistrationError(providerID string) error {
	return c.providerRegistrationError(c.providerCapabilities(), providerID)
}

func (c *Config) providerRegistrationError(registry *providerregistry.Registry, providerID string) error {
	if c == nil || c.Providers == nil || registry == nil {
		return nil
	}
	provider, configured := c.Providers.Get(providerID)
	if !configured || provider.Plugin == nil {
		return nil
	}
	registration, ok := registry.Lookup(providerID)
	if !ok || registration.ProviderID != providerID || !pluginReferenceMatches(provider.Plugin, registration) {
		return nil
	}
	_, err := providerregistry.BindRegistrationConfiguration(registration, provider.Configuration)
	return err
}

func activeProviderPresetForRegistry(cfg *Config, registry *providerregistry.Registry, providerID string) (ProviderPresetReference, bool) {
	var reference ProviderPresetReference
	var active bool
	if cfg == nil {
		reference, active = ActiveProviderPreset(providerID)
	} else {
		reference, active = cfg.activeProviderPreset(providerID)
	}
	if !active {
		return ProviderPresetReference{}, false
	}
	if registry != nil {
		if registration, registered := registry.Lookup(providerID); registered && registration.ProviderID == providerID {
			return ProviderPresetReference{}, false
		}
	}
	return reference, true
}

func (c *Config) providerPreset(registry *providerregistry.Registry, providerID string) (ProviderPresetReference, bool) {
	if c == nil {
		return ProviderPresetReference{}, false
	}
	reference, active := activeProviderPresetForRegistry(c, registry, providerID)
	if !active {
		return ProviderPresetReference{}, false
	}
	if c.Providers != nil {
		provider, configured := c.Providers.Get(providerID)
		if configured {
			if provider.Owner != nil && (provider.Owner.Type != ProviderOwnerPreset ||
				provider.Owner.Construction != providerregistry.ConstructionOpenAICompat ||
				provider.Owner.CompatibilityAdapter != "") {
				return ProviderPresetReference{}, false
			}
			if !providerPresetReferenceMatches(providerID, provider.Preset, reference) {
				return ProviderPresetReference{}, false
			}
			return reference, true
		}
	}
	return reference, true
}

func (c *Config) ProviderPreset(providerID string) (ProviderPresetReference, bool) {
	return c.providerPreset(c.providerCapabilities(), providerID)
}

func (c *Config) ProviderRegistrations() []providerregistry.Registration {
	var result []providerregistry.Registration
	for _, registration := range c.providerCapabilities().Registrations() {
		if exact, ok := c.ProviderRegistration(registration.ProviderID); ok {
			result = append(result, exact)
		}
	}
	return result
}

func (c *Config) ProviderAccountNamespaces() []string {
	if c == nil {
		return nil
	}
	registrations := c.ProviderRegistrations()
	slices.SortFunc(registrations, func(a, b providerregistry.Registration) int {
		if a.AccountOrder != b.AccountOrder {
			return cmp.Compare(a.AccountOrder, b.AccountOrder)
		}
		return strings.Compare(a.AccountNamespace, b.AccountNamespace)
	})
	result := make([]string, 0, len(registrations))
	for _, registration := range registrations {
		if registration.AccountNamespace != "" {
			result = append(result, registration.AccountNamespace)
		}
	}
	return result
}

func (c *Config) ProviderRegistrationForAccount(name string) (providerregistry.Registration, bool) {
	registry := c.providerCapabilities()
	if registration, ok := registry.Lookup(name); ok {
		if exact, active := c.ProviderRegistration(registration.ProviderID); active && exact.AccountNamespace != "" {
			return exact, true
		}
	}
	for _, registration := range registry.Registrations() {
		if registration.AccountNamespace != name && !slices.Contains(registration.AccountAliases, name) {
			continue
		}
		if exact, ok := c.ProviderRegistration(registration.ProviderID); ok && exact.AccountNamespace != "" {
			return exact, true
		}
	}
	return providerregistry.Registration{}, false
}

func (c *Config) ProviderBehaviorRegistration(providerID string) (providerregistry.Registration, bool) {
	return c.providerRegistration(c.providerCapabilities(), providerID)
}

// IsProviderIntegrationAvailable returns false when a configured provider's
// durable plugin or non-compat OAuth owner is not active in this host
// generation. It intentionally ignores the user's disable flag so disabled
// providers can remain visible in controls that allow re-enabling them. A false
// result means "retain but cannot construct," not "replace with a default."
func (c *Config) IsProviderIntegrationAvailable(provider string) bool {
	_, ok := c.Providers.Get(provider)
	return ok && !c.isUnavailableRegisteredProvider(provider)
}

// IsProviderAvailable returns false for disabled providers and for providers
// whose durable plugin or non-compat OAuth owner is not active in this host
// generation. Availability controls construction only; it does not own or
// rewrite the user's persisted provider and model selection.
func (c *Config) IsProviderAvailable(provider string) bool {
	providerConfig, ok := c.Providers.Get(provider)
	return ok && !providerConfig.Disable && c.IsProviderIntegrationAvailable(provider)
}

// IsModelAvailable returns true if the exact provider integration is available
// and the model exists in that provider's catalog. Keep both checks exact: a
// same-named model or an available default from another provider is not a valid
// substitute for an explicit plugin-backed selection.
func (c *Config) IsModelAvailable(provider, model string) bool {
	providerConfig, ok := c.Providers.Get(provider)
	if !ok || !c.IsProviderAvailable(provider) {
		return false
	}
	for _, m := range providerConfig.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

// GetProviderForModel returns the provider named by the selected model slot.
// It must not fall through to another configured provider when the selected
// plugin is unavailable; construction should fail clearly while selection and
// provider-specific values remain intact.
func (c *Config) GetProviderForModel(modelType SelectedModelType) *ProviderConfig {
	model, ok := c.Models[modelType]
	if !ok {
		return nil
	}
	if providerConfig, ok := c.Providers.Get(model.Provider); ok {
		return &providerConfig
	}
	return nil
}

func (c *Config) GetModelByType(modelType SelectedModelType) *catalog.Model {
	model, ok := c.Models[modelType]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) LargeModel() *catalog.Model {
	model, ok := c.Models[SelectedModelTypeLarge]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) SmallModel() *catalog.Model {
	model, ok := c.Models[SelectedModelTypeSmall]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

const maxRecentModelsPerType = 5

func allToolNames() []string {
	return []string{
		"agent",
		"bash",
		"imagegen",
		"jq",
		"crux_info",
		"crux_logs",
		"traffic_logs",
		"traffic_capture",
		"git_inspect",
		"job_list",
		"job_output",
		"job_kill",
		"task_list",
		"task_output",
		"task_stop",
		"task_continue",
		"download",
		"edit",
		"multiedit",
		"lsp_diagnostics",
		"lsp_references",
		"lsp_restart",
		"lsp_symbols",
		"lsp_definition",
		"lsp_call_hierarchy",
		"lsp_rename",
		"lsp_replace_symbol",
		"fetch",
		"agentic_fetch",
		"codebase_search",
		"complete_plan",
		"search",
		"ls",
		"memory_list",
		"memory_upsert",
		"memory_remove",
		"enter_plan",
		"exit_plan",
		"skill_list",
		"skill_load",
		"project_complete",
		"project_create",
		"project_notes",
		"project_status",
		"project_update",
		"question",
		"script",
		"sourcegraph",
		"todos",
		"view",
		"write",
		"list_mcp_resources",
		"read_mcp_resource",
	}
}

func resolveAllowedTools(allTools []string, disabledTools []string) []string {
	if disabledTools == nil {
		return allTools
	}
	// filter out disabled tools (exclude mode)
	return filterSlice(allTools, disabledTools, false)
}

func resolveReadOnlyTools(tools []string) []string {
	readOnlyTools := []string{"codebase_search", "git_inspect", "job_list", "job_output", "jq", "ls", "lsp_call_hierarchy", "lsp_definition", "lsp_symbols", "memory_list", "project_status", "search", "skill_list", "skill_load", "sourcegraph", "task_list", "task_output", "view"}
	// filter to only include tools that are in allowedtools (include mode)
	return filterSlice(tools, readOnlyTools, true)
}

func filterSlice(data []string, mask []string, include bool) []string {
	var filtered []string
	for _, s := range data {
		// if include is true, we include items that ARE in the mask
		// if include is false, we include items that are NOT in the mask
		if include == slices.Contains(mask, s) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func (c *Config) SetupAgents() {
	allowedTools := resolveAllowedTools(allToolNames(), c.Options.DisabledTools)

	agents := map[string]Agent{
		AgentCoder: {
			ID:           AgentCoder,
			Name:         "Coder",
			Description:  "An agent that helps with executing coding tasks.",
			Model:        SelectedModelTypeLarge,
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: allowedTools,
		},

		AgentTask: {
			ID:           AgentTask,
			Name:         "Task",
			Description:  "An agent that helps with searching for context and finding implementation details.",
			Model:        SelectedModelTypeLarge,
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: resolveReadOnlyTools(allowedTools),
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},
	}
	c.Agents = agents
}

func (c *ProviderConfig) TestConnection(ctx context.Context, resolver VariableResolver, validate providertransport.OwnerValidator) error {
	if validate == nil {
		return fmt.Errorf("provider owner validator is unavailable")
	}
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return err
	}
	if err := validateConfiguredProviderOwner(c.ID, *c); err != nil {
		return err
	}

	providerID := catalog.ProviderID(c.ID)
	apiKey, _ := resolver.ResolveValue(c.APIKey)
	exactPreset := c.Owner.Type == ProviderOwnerPreset
	if exactPreset && (c.Preset.ID == "" || c.Preset.Version == "" || c.Preset.Digest == "") {
		return fmt.Errorf("provider preset for provider %s has an incomplete owner reference", c.ID)
	}

	switch {
	case exactPreset && (providerID == catalog.ProviderMiniMax || providerID == catalog.ProviderMiniMaxChina):
		return nil
	case exactPreset && providerID == catalog.ProviderAlibabaSingapore:
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	providerType := cmp.Or(c.Type, catalog.TypeOpenAICompat)
	if providerType != catalog.TypeOpenAICompat && !discover.IsKnownCustomProvider(string(providerType)) {
		return fmt.Errorf("unsupported provider type %q", providerType)
	}
	baseURL, err := resolver.ResolveValue(c.BaseURL)
	if err != nil {
		return fmt.Errorf("resolve provider %s base URL: %w", c.ID, err)
	}
	if baseURL == "" {
		return fmt.Errorf("provider %s is missing an API endpoint", c.ID)
	}
	testURL := strings.TrimRight(baseURL, "/") + "/models"
	if exactPreset && providerID == catalog.ProviderOpenCodeGo {
		testURL = strings.TrimRight(strings.Replace(baseURL, "/go", "", 1), "/") + "/models"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for key, value := range c.ExtraHeaders {
		req.Header.Set(key, value)
	}

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
	if ownerErr := providertransport.ValidateContextOwner(ctx); ownerErr != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return ownerErr
	}
	if err != nil {
		return fmt.Errorf("failed to connect to provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	if exactPreset && providerID == catalog.ProviderZAI {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
	}
	return nil
}

// resolveEnvs expands every value in envs through the given resolver
// and returns a fresh "KEY=value" slice sorted by key. The input map is
// not mutated. On the first resolution failure it returns nil and an
// error identifying the offending variable; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w.
func resolveEnvs(envs map[string]string, r VariableResolver) ([]string, error) {
	if len(envs) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res := make([]string, 0, len(envs))
	for _, k := range keys {
		v, err := r.ResolveValue(envs[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res, nil
}

func ptrValOr[T any](t *T, el T) T {
	if t == nil {
		return el
	}
	return *t
}
