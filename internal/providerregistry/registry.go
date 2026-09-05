// Package providerregistry defines the host-owned provider capability registry.
// Plugin bundles contribute declarative manifest values; temporary integrated
// behavior adapters remain core-owned and are selected by registration, never by
// bundle code.
package providerregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/anthropic"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/oauth/gemini/antigravity"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

// Construction identifies a host-owned construction path. Values beginning
// with "integrated-" are temporary compatibility adapters; protocol values are
// generic core transports selected from declarative manifests.
type Construction string

type QuotaCredential string

// OwnerMode is an explicit host rollout choice for one logical provider.
// Bundle discovery never changes this value.
type OwnerMode string

const (
	OwnerIntegrated   OwnerMode = "integrated"
	OwnerPluginCompat OwnerMode = "plugin-compat"
	OwnerPluginNative OwnerMode = "plugin-native"
	OwnerDisabled     OwnerMode = "disabled"

	ConstructionCodex             Construction = "integrated-codex"
	ConstructionGeminiAntigravity Construction = "integrated-gemini-antigravity"
	ConstructionCopilot           Construction = "integrated-copilot"
	ConstructionAnthropicMessages Construction = "anthropic-messages"
	ConstructionOpenAIResponses   Construction = "openai-responses"
	ConstructionGeminiContent     Construction = "gemini-generate-content"
	ConstructionGeminiInteraction Construction = "gemini-interactions"
	ConstructionGenericJSON       Construction = "generic-json"
	ConstructionOpenAICompat      Construction = "openai-compat"

	QuotaCredentialAccessToken  QuotaCredential = "access-token"
	QuotaCredentialRefreshToken QuotaCredential = "refresh-token"
)

// LoginAdapter identifies a host-owned interactive flow implementation.
type LoginAdapter string

const (
	LoginBrowser     LoginAdapter = "browser"
	LoginHostedPaste LoginAdapter = "hosted-paste"
	LoginDeviceCode  LoginAdapter = "device-code"
)

// OpenURL presents an authorization URL to the user.
type OpenURL func(string) error

// ReadCode obtains a hosted redirect value pasted by the user.
type ReadCode func() (string, error)

// DeviceAuthorization contains display-safe device-flow values plus opaque
// host-owned polling state. Declarative plugins never provide State values.
type DeviceAuthorization struct {
	UserCode        string
	VerificationURL string
	State           any
}

// OAuthCapability contains host-owned OAuth behavior and declarative metadata.
// Manifest callbacks execute only finite host interpreters; compatibility or
// core-owned providers may supply audited callbacks for unsupported flow kinds.
type OAuthCapability struct {
	Adapter           LoginAdapter
	FlowID            string
	Authorize         func(context.Context, OpenURL, ReadCode) (*oauth.Token, error)
	Import            func(context.Context) (*oauth.Token, bool, error)
	RequestDeviceCode func(context.Context) (*DeviceAuthorization, error)
	PollDeviceCode    func(context.Context, *DeviceAuthorization) (*oauth.Token, error)
	Refresh           func(context.Context, string) (*oauth.Token, error)
}

// Identity resolves a stable account label and optional opaque account metadata.
type Identity func(context.Context, string) (id, display string, raw json.RawMessage)

// InstructionCapability contains resolved, immutable UTF-8 profile text. Bundle
// paths are consumed during secure validation and never reopened by consumers.
type InstructionCapability struct {
	Default          string
	SelectionDefault string
	Profiles         map[string]string
	HiddenSkills     []string
}

// ReasoningCapability binds generic fallback state to provider-neutral option
// conversion selected by a registered protocol policy.
type ReasoningCapability struct {
	FallbackOnUnsupported bool
	Options               func(modelID, effort string, canReason bool, merged map[string]any) (fantasy.ProviderOptions, error)
	Disable               func(fantasy.ProviderOptions) fantasy.ProviderOptions
}

// RuntimeValues are host-owned configured values supplied to a registered
// runtime-control adapter.
type RuntimeValues struct {
	ResponseVerbosity string
	AnalysisEffort    string
}

// RuntimeCapability binds declarative control metadata to core-owned storage
// and provider-option application.
type RuntimeCapability struct {
	Available func(modelID string) bool
	Apply     func(RuntimeValues, fantasy.ProviderOptions) fantasy.ProviderOptions
}

type RegistrationOwner struct {
	ProviderID           string       `json:"provider_id"`
	AccountNamespace     string       `json:"account_namespace,omitempty"`
	Construction         Construction `json:"construction,omitempty"`
	CompatibilityAdapter Construction `json:"compatibility_adapter,omitempty"`
	HasOAuth             bool         `json:"has_oauth,omitempty"`
	OAuthAdapter         LoginAdapter `json:"oauth_adapter,omitempty"`
	OAuthFlowID          string       `json:"oauth_flow_id,omitempty"`
	HasManifest          bool         `json:"has_manifest,omitempty"`
	ManifestID           string       `json:"manifest_id,omitempty"`
	ManifestVersion      string       `json:"manifest_version,omitempty"`
	HasPreset            bool         `json:"has_preset,omitempty"`
	PresetID             string       `json:"preset_id,omitempty"`
	PresetVersion        string       `json:"preset_version,omitempty"`
	PresetDigest         string       `json:"preset_digest,omitempty"`
}

func (r Registration) Owner() RegistrationOwner {
	owner := RegistrationOwner{
		ProviderID:           r.ProviderID,
		AccountNamespace:     r.AccountNamespace,
		Construction:         r.Construction,
		CompatibilityAdapter: r.CompatibilityAdapter,
		HasOAuth:             r.OAuth != nil,
		HasManifest:          r.Manifest != nil,
	}
	if r.OAuth != nil {
		owner.OAuthAdapter = r.OAuth.Adapter
		owner.OAuthFlowID = r.OAuth.FlowID
	}
	if r.Manifest != nil {
		owner.ManifestID = r.Manifest.ID
		owner.ManifestVersion = r.Manifest.Version
	}
	return owner
}

func (o RegistrationOwner) Matches(registration Registration) bool {
	return o == registration.Owner()
}

// Registration is one immutable logical provider capability registration.
type Registration struct {
	ProviderID           string
	Name                 string
	Brand                *Brand
	Aliases              []string
	AccountNamespace     string
	AccountAliases       []string
	Construction         Construction
	CompatibilityAdapter Construction
	Operation            *providertransport.Operation
	Operations           map[string]*providertransport.Operation
	OAuth                *OAuthCapability
	Identity             Identity
	Quota                oauthusage.Fetcher
	QuotaCredential      QuotaCredential
	Usage                *manifest.UsagePolicy
	Images               *manifest.ImagePolicy
	Instructions         *InstructionCapability
	RuntimeControls      []manifest.RuntimeControl
	Metadata             []manifest.MetadataContract
	Runtime              *RuntimeCapability
	Reasoning            *ReasoningCapability
	Errors               []manifest.ErrorMapping
	Manifest             *manifest.Manifest
	LoginOrder           int
	AccountOrder         int
}

// Clone returns a detached registration so consumers cannot mutate manifest
// operations, identity policy, OAuth behavior, runtime controls, or error
// mappings held by the active registry. Shallow copies here create cross-client
// behavior changes that are extremely difficult to attribute to a plugin.
func (r Registration) Clone() Registration {
	r.Aliases = slices.Clone(r.Aliases)
	if r.Brand != nil {
		value := *r.Brand
		r.Brand = &value
	}
	r.AccountAliases = slices.Clone(r.AccountAliases)
	r.Operation = r.Operation.Clone()
	if r.Operations != nil {
		operations := r.Operations
		r.Operations = make(map[string]*providertransport.Operation, len(operations))
		for id, operation := range operations {
			r.Operations[id] = operation.Clone()
		}
	}
	if r.OAuth != nil {
		value := *r.OAuth
		r.OAuth = &value
	}
	if r.Usage != nil {
		value := cloneJSON(*r.Usage)
		r.Usage = &value
	}
	if r.Images != nil {
		value := cloneJSON(*r.Images)
		r.Images = &value
	}
	if r.Instructions != nil {
		value := *r.Instructions
		value.Profiles = maps.Clone(r.Instructions.Profiles)
		value.HiddenSkills = slices.Clone(r.Instructions.HiddenSkills)
		r.Instructions = &value
	}
	r.RuntimeControls = cloneJSON(r.RuntimeControls)
	r.Metadata = cloneJSON(r.Metadata)
	if r.Runtime != nil {
		value := *r.Runtime
		r.Runtime = &value
	}
	if r.Reasoning != nil {
		value := *r.Reasoning
		r.Reasoning = &value
	}
	r.Errors = cloneJSON(r.Errors)
	if r.Manifest != nil {
		value := cloneManifest(*r.Manifest)
		r.Manifest = &value
	}
	return r
}

// Registry is an immutable provider-ID and alias lookup.
type Registry struct {
	providers map[string]Registration
	aliases   map[string]string
}

// New builds an immutable provider registry without publishing any runtime
// account or refresh state. Candidate registries remain private until the
// configuration generation that owns them is committed.
func New(registrations ...Registration) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Registration), aliases: make(map[string]string)}
	for _, registration := range registrations {
		if err := ValidateActivation(registration); err != nil {
			return nil, err
		}
		if err := registry.add(registration); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// add enforces one canonical owner per provider ID, alias, and account
// namespace before cloning the registration into registry state. These checks
// prevent one plugin from shadowing another provider's construction or OAuth
// identity.
func (r *Registry) add(registration Registration) error {
	if registration.ProviderID == "" {
		return fmt.Errorf("provider registration has no ID")
	}
	if _, exists := r.providers[registration.ProviderID]; exists {
		return fmt.Errorf("provider %q is registered more than once", registration.ProviderID)
	}
	registration = registration.Clone()
	if registration.AccountNamespace != "" {
		for _, current := range r.providers {
			if current.AccountNamespace == registration.AccountNamespace {
				return fmt.Errorf("account namespace %q is claimed by %q and %q", registration.AccountNamespace, current.ProviderID, registration.ProviderID)
			}
		}
	}
	r.providers[registration.ProviderID] = registration
	for _, alias := range append([]string{registration.ProviderID}, registration.Aliases...) {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" {
			continue
		}
		if owner, exists := r.aliases[alias]; exists && owner != registration.ProviderID {
			return fmt.Errorf("provider alias %q is claimed by %q and %q", alias, owner, registration.ProviderID)
		}
		r.aliases[alias] = registration.ProviderID
	}
	return nil
}

// Lookup resolves an exact canonical provider or declared alias and returns a
// clone. It must not guess nearby provider IDs or fall back to another
// registration, because callers use the result to select constructors,
// transports, identity policy, and credentials.
func (r *Registry) Lookup(providerOrAlias string) (Registration, bool) {
	if r == nil {
		return Registration{}, false
	}
	id := strings.ToLower(strings.TrimSpace(providerOrAlias))
	if canonical, ok := r.aliases[id]; ok {
		id = canonical
	}
	registration, ok := r.providers[id]
	return registration.Clone(), ok
}

// HasAccountNamespace reports whether an active registration owns the stored
// account namespace, including a declared legacy alias. Keep this tied to
// active registration ownership so credentials are never handed to a provider
// selected merely by a similar ID.
func (r *Registry) HasAccountNamespace(namespace string) bool {
	if r == nil || namespace == "" {
		return false
	}
	for _, registration := range r.providers {
		if registration.AccountNamespace == namespace || slices.Contains(registration.AccountAliases, namespace) {
			return true
		}
	}
	return false
}

// Registrations returns sorted detached registrations for UI and runtime
// projection. Returning registry-owned values would let presentation code
// mutate active plugin behavior.
func (r *Registry) AccountRegistrations() []accounts.ProviderRegistration {
	if r == nil {
		return nil
	}
	result := make([]accounts.ProviderRegistration, 0, len(r.providers))
	for _, registration := range r.providers {
		if registration.AccountNamespace == "" {
			continue
		}
		var refresher accounts.Refresher
		if registration.OAuth != nil {
			refresher = registration.OAuth.Refresh
		}
		result = append(result, accounts.ProviderRegistration{
			ProviderID: registration.ProviderID,
			Namespace:  registration.AccountNamespace,
			Aliases:    slices.Clone(registration.AccountAliases),
			Order:      registration.AccountOrder,
			Refresher:  refresher,
		})
	}
	return result
}

func (r *Registry) Registrations() []Registration {
	if r == nil {
		return nil
	}
	result := make([]Registration, 0, len(r.providers))
	for _, registration := range r.providers {
		result = append(result, registration.Clone())
	}
	slices.SortFunc(result, func(a, b Registration) int {
		if a.LoginOrder != b.LoginOrder {
			return a.LoginOrder - b.LoginOrder
		}
		return strings.Compare(a.ProviderID, b.ProviderID)
	})
	return result
}

// Clone snapshots the full active registry for configuration and workspace
// boundaries. Operations and manifest policy must remain deep-cloned so reloads
// cannot mutate providers already serving requests.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return nil
	}
	result := &Registry{providers: make(map[string]Registration, len(r.providers)), aliases: maps.Clone(r.aliases)}
	for id, registration := range r.providers {
		result.providers[id] = registration.Clone()
	}
	return result
}

// Integrated returns the current core-owned compatibility registrations.
// Copilot remains core-owned because codebase indexing depends on it.
func Integrated() []Registration {
	return []Registration{
		{
			ProviderID: gemini.ID, Name: gemini.Name,
			Brand:   &Brand{Label: "Gemini", ShortName: "GEMINI", Color: "#8CE99A", GradientA: "#1E8E3E", GradientB: "#8CE99A"},
			Aliases: []string{"gemini", "antigravity"}, AccountNamespace: accounts.ProviderGemini,
			Construction: ConstructionGeminiAntigravity, LoginOrder: 20, AccountOrder: 40,
			Images:    integratedImagePolicy(0, 0, 0, "", "none"),
			Reasoning: &ReasoningCapability{FallbackOnUnsupported: true, Options: geminiReasoningOptions, Disable: disableGeminiReasoning},
			OAuth: &OAuthCapability{
				Adapter: LoginHostedPaste, FlowID: "gemini-antigravity",
				Authorize: func(ctx context.Context, open OpenURL, read ReadCode) (*oauth.Token, error) {
					if read == nil {
						return nil, fmt.Errorf("provider %s requires pasted authorization input", gemini.ID)
					}
					return gemini.Authorize(ctx, open, read)
				},
				Refresh: gemini.Refresh,
			},
			Identity: func(ctx context.Context, accessToken string) (string, string, json.RawMessage) {
				email := gemini.AccountEmail(ctx, accessToken)
				return email, email, nil
			},
		},
		{
			ProviderID: codex.ID, Name: codex.Name,
			Brand:   &Brand{Label: "Codex", ShortName: "CODEX", Color: "#7FC4FF", GradientA: "#1B3B8B", GradientB: "#7FC4FF"},
			Aliases: []string{"chatgpt", "openai"}, AccountNamespace: accounts.ProviderCodex,
			Construction: ConstructionCodex, LoginOrder: 30, AccountOrder: 20,
			Images:          integratedCodexImagePolicy(),
			Instructions:    integratedInstructions("native", codex.StandardToolingInstructions()),
			RuntimeControls: codexRuntimeControls(), Runtime: codexRuntimeCapability(),
			Reasoning: &ReasoningCapability{FallbackOnUnsupported: true, Options: codexReasoningOptions, Disable: disableCodexReasoning},
			Quota:     oauthusage.FetchCodex,
			OAuth: &OAuthCapability{
				Adapter: LoginBrowser, FlowID: "codex",
				Authorize: func(ctx context.Context, open OpenURL, _ ReadCode) (*oauth.Token, error) {
					return codex.Authorize(ctx, open)
				},
				Refresh: codex.RefreshToken,
			},
			Identity: func(ctx context.Context, accessToken string) (string, string, json.RawMessage) {
				email := codex.AccountEmail(ctx, accessToken)
				accountID := codex.AccountID(accessToken)
				id := email
				if id == "" {
					id = accountID
				}
				var raw json.RawMessage
				if accountID != "" {
					raw, _ = json.Marshal(map[string]string{"account_id": accountID})
				}
				return id, id, raw
			},
		},
		{
			ProviderID: "copilot", Name: "GitHub Copilot",
			Brand:   &Brand{Label: "GitHub Copilot", ShortName: "COPILOT", Color: "#C9A8FF", GradientA: "#C9A8FF", GradientB: "#C9A8FF"},
			Aliases: []string{"github", "github-copilot"}, AccountNamespace: accounts.ProviderCopilot,
			Construction: ConstructionCopilot, LoginOrder: 40, AccountOrder: 30,
			Quota: oauthusage.FetchCopilot, QuotaCredential: QuotaCredentialRefreshToken,
			OAuth: &OAuthCapability{
				Adapter: LoginDeviceCode, FlowID: "github-copilot", Refresh: copilot.RefreshToken,
				Import: func(ctx context.Context) (*oauth.Token, bool, error) {
					refreshToken, ok := copilot.RefreshTokenFromDisk()
					if !ok {
						return nil, false, nil
					}
					token, err := copilot.RefreshToken(ctx, refreshToken)
					return token, true, err
				},
				RequestDeviceCode: func(ctx context.Context) (*DeviceAuthorization, error) {
					code, err := copilot.RequestDeviceCode(ctx)
					if err != nil {
						return nil, err
					}
					return &DeviceAuthorization{UserCode: code.UserCode, VerificationURL: code.VerificationURI, State: code}, nil
				},
				PollDeviceCode: func(ctx context.Context, authorization *DeviceAuthorization) (*oauth.Token, error) {
					code, ok := authorization.State.(*copilot.DeviceCode)
					if !ok || code == nil {
						return nil, fmt.Errorf("invalid GitHub Copilot device authorization state")
					}
					return copilot.PollForToken(ctx, code)
				},
			},
		},
	}
}

// SelectOwners resolves exactly one runtime owner per logical provider. The
// default is integrated ownership for integrated providers and plugin ownership
// for non-conflicting plugins. Explicit disabled, compatibility, and native
// modes fail closed. Never substitute a different owner when the requested
// plugin mode is unavailable; ownership controls construction, OAuth, identity,
// transport, usage, runtime options, and reasoning behavior as one unit.
func SelectOwners(integrated, plugins []Registration, modes map[string]OwnerMode) ([]Registration, error) {
	pluginByID := make(map[string]Registration, len(plugins))
	for _, plugin := range plugins {
		if _, exists := pluginByID[plugin.ProviderID]; exists {
			return nil, fmt.Errorf("plugin provider %q is registered more than once", plugin.ProviderID)
		}
		pluginByID[plugin.ProviderID] = plugin
	}

	result := make([]Registration, 0, len(integrated)+len(plugins))
	owned := make(map[string]struct{}, len(integrated)+len(plugins))
	for _, current := range integrated {
		selected := current
		switch modes[current.ProviderID] {
		case "", OwnerIntegrated:
		case OwnerDisabled:
			continue
		case OwnerPluginCompat:
			plugin, ok := pluginByID[current.ProviderID]
			if !ok {
				return nil, fmt.Errorf("provider %q selects plugin-compat without a registered plugin", current.ProviderID)
			}
			if plugin.CompatibilityAdapter != current.Construction {
				return nil, fmt.Errorf("provider %q plugin compatibility adapter %q does not match integrated owner %q", current.ProviderID, plugin.CompatibilityAdapter, current.Construction)
			}
			selected = plugin
			delete(pluginByID, current.ProviderID)
		case OwnerPluginNative:
			plugin, ok := pluginByID[current.ProviderID]
			if !ok {
				return nil, fmt.Errorf("provider %q selects plugin-native without a registered plugin", current.ProviderID)
			}
			if plugin.CompatibilityAdapter != "" {
				return nil, fmt.Errorf("provider %q selects plugin-native but still requires compatibility adapter %q", current.ProviderID, plugin.CompatibilityAdapter)
			}
			selected = plugin
			delete(pluginByID, current.ProviderID)
		default:
			return nil, fmt.Errorf("provider %q has unknown owner mode %q", current.ProviderID, modes[current.ProviderID])
		}
		result = append(result, selected)
		owned[selected.ProviderID] = struct{}{}
	}
	for _, plugin := range plugins {
		if _, exists := owned[plugin.ProviderID]; exists {
			continue
		}
		if _, conflict := pluginByID[plugin.ProviderID]; !conflict {
			continue
		}
		switch modes[plugin.ProviderID] {
		case OwnerDisabled, OwnerIntegrated:
			continue
		case OwnerPluginCompat:
			if plugin.CompatibilityAdapter == "" {
				return nil, fmt.Errorf("provider %q selects plugin-compat but the plugin is native", plugin.ProviderID)
			}
		case OwnerPluginNative:
			if plugin.CompatibilityAdapter != "" {
				return nil, fmt.Errorf("provider %q selects plugin-native but still requires compatibility adapter %q", plugin.ProviderID, plugin.CompatibilityAdapter)
			}
		case "":
		default:
			return nil, fmt.Errorf("provider %q has unknown owner mode %q", plugin.ProviderID, modes[plugin.ProviderID])
		}
		result = append(result, plugin)
		owned[plugin.ProviderID] = struct{}{}
	}
	return result, nil
}

// FromManifest is the complete trusted-manifest-to-runtime projection. It
// compiles every operation and carries construction, OAuth, identity, usage,
// images, instructions, runtime controls, reasoning, errors, and ordering into
// one registration. Do not project only the fields a current built-in provider
// happens to need; private plugins rely on host capabilities independently of
// the public provider catalog.
func FromManifest(value manifest.Manifest, staticFiles ...map[string]string) (Registration, error) {
	var inference *manifest.Operation
	for i := range value.Capabilities.Operations {
		if value.Capabilities.Operations[i].Kind == "inference" {
			inference = &value.Capabilities.Operations[i]
			break
		}
	}
	if inference == nil {
		return Registration{}, fmt.Errorf("provider %q has no inference operation", value.Provider.ID)
	}
	operations := make(map[string]*providertransport.Operation, len(value.Capabilities.Operations))
	for _, declaration := range value.Capabilities.Operations {
		compiled, err := providertransport.Compile(value, declaration)
		if err != nil {
			return Registration{}, fmt.Errorf("compile provider %q operation %q: %w", value.Provider.ID, declaration.ID, err)
		}
		operations[declaration.ID] = compiled
	}
	compiledOperation := operations[inference.ID]
	var instructionProfiles map[string]string
	if policy := value.Capabilities.Instructions; policy != nil {
		files := map[string]string(nil)
		if len(staticFiles) > 0 {
			files = staticFiles[0]
		}
		instructionProfiles = make(map[string]string, len(policy.Profiles))
		for id, path := range policy.Profiles {
			text, ok := files[path]
			if !ok {
				return Registration{}, fmt.Errorf("provider %q instruction profile %q has no validated static text", value.Provider.ID, id)
			}
			instructionProfiles[id] = text
		}
	}
	if policy := value.Capabilities.Anthropic; policy != nil && policy.InstructionBlock != nil {
		text, ok := instructionProfiles[policy.InstructionBlock.Profile]
		if !ok {
			return Registration{}, fmt.Errorf("provider %q Anthropic instruction block references unresolved profile %q", value.Provider.ID, policy.InstructionBlock.Profile)
		}
		var cache *manifest.AnthropicCacheControl
		if policy.InstructionBlock.CacheControl != nil {
			value := cloneJSON(*policy.InstructionBlock.CacheControl)
			cache = &value
		}
		compiledOperation.SystemInstruction = &providertransport.ResolvedSystemInstruction{Text: text, CacheControl: cache}
	}
	registration := Registration{
		ProviderID: value.Provider.ID, Name: value.Provider.Name,
		Aliases:          slices.Clone(value.Provider.Aliases),
		AccountNamespace: value.Provider.AccountNamespace,
		AccountAliases:   slices.Clone(value.Provider.LegacyAccountAliases),
		Construction:     Construction(inference.Protocol),
		Operation:        compiledOperation,
		Operations:       operations,
		RuntimeControls:  cloneJSON(value.Capabilities.RuntimeControls),
		Metadata:         cloneJSON(value.Capabilities.Metadata),
		Errors:           cloneJSON(value.Capabilities.Errors),
		Manifest:         ptr(cloneManifest(value)),
		LoginOrder:       value.Provider.LoginOrder,
		AccountOrder:     value.Provider.AccountOrder,
	}
	if value.Provider.Brand != nil {
		registration.Brand = &Brand{
			Label: value.Provider.Brand.Label, ShortName: value.Provider.Brand.ShortName,
			Color: value.Provider.Brand.Color, GradientA: value.Provider.Brand.GradientA, GradientB: value.Provider.Brand.GradientB,
		}
	}
	if value.Capabilities.Usage != nil {
		policy := cloneJSON(*value.Capabilities.Usage)
		registration.Usage = &policy
		if policy.Source == "operation" {
			fetcher, err := oauthusage.ManifestFetcher(operations, policy)
			if err != nil {
				return Registration{}, fmt.Errorf("compile provider %q usage: %w", value.Provider.ID, err)
			}
			registration.Quota = fetcher
		}
	}
	if policy := value.Capabilities.Anthropic; policy != nil && policy.ReasoningFallback {
		registration.Reasoning = &ReasoningCapability{FallbackOnUnsupported: true, Disable: disableAnthropicReasoning}
	}
	if value.Capabilities.Images != nil {
		policy := cloneJSON(*value.Capabilities.Images)
		registration.Images = &policy
	}
	for _, declaration := range value.Capabilities.Operations {
		if declaration.Kind == "account" {
			registration.Identity = providertransport.AccountIdentity(operations[declaration.ID], value.Capabilities.Credentials)
			break
		}
	}
	if len(value.Capabilities.RuntimeControls) > 0 {
		registration.Runtime = declarativeRuntimeCapability(value.Provider.ID, registration.Construction, value.Capabilities.RuntimeControls)
	}
	if registration.Reasoning == nil && len(value.Capabilities.RuntimeControls) > 0 {
		registration.Reasoning = declarativeReasoningCapability(value.Provider.ID, registration.Construction, value.Capabilities.RuntimeControls)
	}
	if policy := value.Capabilities.Instructions; policy != nil {
		registration.Instructions = &InstructionCapability{
			Default:          policy.Default,
			SelectionDefault: policy.SelectionDefault,
			Profiles:         instructionProfiles,
			HiddenSkills:     slices.Clone(policy.HiddenSkills),
		}
	}
	if len(value.Capabilities.OAuth) > 0 {
		oauthCapability, oauthErr := manifestOAuthCapability(value, value.Capabilities.OAuth[0])
		if oauthErr != nil {
			return Registration{}, oauthErr
		}
		registration.OAuth = oauthCapability
	}
	if compatibility := value.Capabilities.Compatibility; compatibility != nil {
		if err := attachCompatibilityAdapter(&registration, *compatibility); err != nil {
			return Registration{}, err
		}
	}
	return registration, nil
}

// attachCompatibilityAdapter delegates only explicitly named capabilities to
// an integrated adapter owned by the same provider and account namespace. Do
// not wholesale replace the plugin registration or infer delegation: undeclared
// manifest operation and identity policy must remain plugin-owned.
func attachCompatibilityAdapter(registration *Registration, declaration manifest.CompatibilityAdapter) error {
	adapterID := Construction(declaration.ID)
	var adapter *Registration
	for _, candidate := range Integrated() {
		if candidate.Construction == adapterID {
			value := candidate
			adapter = &value
			break
		}
	}
	if adapter == nil || adapter.Construction == ConstructionCopilot {
		return fmt.Errorf("provider %q names unknown compatibility adapter %q", registration.ProviderID, declaration.ID)
	}
	if adapter.ProviderID != registration.ProviderID {
		return fmt.Errorf("provider %q cannot delegate to adapter %q owned by %q", registration.ProviderID, declaration.ID, adapter.ProviderID)
	}
	if adapter.AccountNamespace != registration.AccountNamespace {
		return fmt.Errorf("provider %q compatibility account namespace %q does not match adapter namespace %q", registration.ProviderID, registration.AccountNamespace, adapter.AccountNamespace)
	}
	registration.CompatibilityAdapter = adapterID
	for _, capability := range declaration.Delegates {
		switch capability {
		case "construction":
			registration.Construction = adapter.Construction
		case "oauth":
			if registration.OAuth == nil || adapter.OAuth == nil {
				return fmt.Errorf("provider %q delegates OAuth without both a declaration and adapter", registration.ProviderID)
			}
			registration.OAuth.Authorize = adapter.OAuth.Authorize
			registration.OAuth.Import = adapter.OAuth.Import
			registration.OAuth.RequestDeviceCode = adapter.OAuth.RequestDeviceCode
			registration.OAuth.PollDeviceCode = adapter.OAuth.PollDeviceCode
			registration.OAuth.Refresh = adapter.OAuth.Refresh
		case "identity":
			registration.Identity = adapter.Identity
		case "usage":
			registration.Quota = adapter.Quota
		case "runtime":
			registration.Runtime = adapter.Runtime
		case "reasoning":
			registration.Reasoning = adapter.Reasoning
		default:
			return fmt.Errorf("provider %q delegates unknown compatibility capability %q", registration.ProviderID, capability)
		}
	}
	return nil
}

func codexRuntimeControls() []manifest.RuntimeControl {
	return []manifest.RuntimeControl{
		{ID: "options.response_verbosity", Label: "Final response verbosity", Type: "enum", Values: []string{"low", "medium", "high"}, Scope: "model", RequestPath: "/text/verbosity"},
		{ID: "options.analysis_effort", Label: "Analysis effort / Juice", Type: "enum", Values: []string{"none", "low", "medium", "high", "xhigh", "max"}, Scope: "model", RequestPath: "/reasoning/effort"},
	}
}

func codexRuntimeCapability() *RuntimeCapability {
	return &RuntimeCapability{
		Available: func(modelID string) bool {
			return modelID == "gpt-5.6" || strings.HasPrefix(modelID, "gpt-5.6-") || modelID == "gpt-6-astra"
		},
		Apply: func(values RuntimeValues, options fantasy.ProviderOptions) fantasy.ProviderOptions {
			result := maps.Clone(options)
			if result == nil {
				result = make(fantasy.ProviderOptions)
			}
			providerOptions := &codexresponses.ProviderOptions{}
			if current, ok := result[codexresponses.Name].(*codexresponses.ProviderOptions); ok {
				clone := *current
				providerOptions = &clone
			}
			if values.ResponseVerbosity != "" {
				providerOptions.ResponseVerbosity = values.ResponseVerbosity
			}
			if values.AnalysisEffort != "" {
				providerOptions.ReasoningEffort = values.AnalysisEffort
				providerOptions.DisableReasoning = false
			}
			if providerOptions.ResponseVerbosity != "" || providerOptions.ReasoningEffort != "" {
				result[codexresponses.Name] = providerOptions
			}
			return result
		},
	}
}

func codexReasoningOptions(_ string, effort string, canReason bool, merged map[string]any) (fantasy.ProviderOptions, error) {
	if configured, ok := merged["reasoning_effort"].(string); ok && configured != "" {
		effort = configured
	}
	if !canReason || effort == "" {
		return fantasy.ProviderOptions{}, nil
	}
	return fantasy.ProviderOptions{codexresponses.Name: &codexresponses.ProviderOptions{ReasoningEffort: effort}}, nil
}

func geminiReasoningOptions(modelID, effort string, canReason bool, merged map[string]any) (fantasy.ProviderOptions, error) {
	if _, exists := merged["thinking_config"]; !exists && canReason {
		if strings.HasPrefix(modelID, "gemini-3") {
			merged["thinking_config"] = map[string]any{"thinking_level": effort, "include_thoughts": true}
		} else {
			merged["thinking_config"] = map[string]any{"thinking_budget": 2000, "include_thoughts": true}
		}
	}
	parsed, err := antigravity.ParseOptions(merged)
	if err != nil {
		return nil, fmt.Errorf("parse Gemini provider options: %w", err)
	}
	return fantasy.ProviderOptions{antigravity.Name: parsed}, nil
}

func disableAnthropicReasoning(options fantasy.ProviderOptions) fantasy.ProviderOptions {
	result := maps.Clone(options)
	if result == nil {
		result = make(fantasy.ProviderOptions)
	}
	providerOptions := &anthropic.ProviderOptions{}
	if current, ok := result[anthropic.Name].(*anthropic.ProviderOptions); ok {
		clone := *current
		providerOptions = &clone
	}
	providerOptions.SendReasoning = nil
	providerOptions.Thinking = nil
	providerOptions.Effort = nil
	providerOptions.ThinkingDisplay = nil
	result[anthropic.Name] = providerOptions
	return result
}

func disableCodexReasoning(options fantasy.ProviderOptions) fantasy.ProviderOptions {
	result := maps.Clone(options)
	if result == nil {
		result = make(fantasy.ProviderOptions)
	}
	providerOptions := &codexresponses.ProviderOptions{DisableReasoning: true}
	if current, ok := result[codexresponses.Name].(*codexresponses.ProviderOptions); ok {
		clone := *current
		providerOptions = &clone
		providerOptions.DisableReasoning = true
	}
	providerOptions.ReasoningEffort = ""
	result[codexresponses.Name] = providerOptions
	return result
}

func disableGeminiReasoning(options fantasy.ProviderOptions) fantasy.ProviderOptions {
	result := maps.Clone(options)
	if result == nil {
		result = make(fantasy.ProviderOptions)
	}
	providerOptions := &antigravity.ProviderOptions{}
	if current, ok := result[antigravity.Name].(*antigravity.ProviderOptions); ok {
		clone := *current
		providerOptions = &clone
	}
	providerOptions.ThinkingConfig = nil
	result[antigravity.Name] = providerOptions
	return result
}

func integratedImagePolicy(maxSide, maxPatches int, maxOutput int64, outputMediaType, flattenAlpha string) *manifest.ImagePolicy {
	return &manifest.ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/jpeg", "image/png", "image/webp"},
		MaxSourceBytes:     25 * 1024 * 1024, MaxSidePixels: maxSide, MaxPatches: maxPatches,
		MaxOutputBytes: maxOutput, OutputMediaType: outputMediaType, FlattenAlpha: flattenAlpha,
		QualitySteps: []int{85, 75, 65, 55, 45, 35, 25}, ResizePercent: 80,
	}
}

func integratedCodexImagePolicy() *manifest.ImagePolicy {
	policy := integratedImagePolicy(1920, 2500, 512*1024, "image/jpeg", "white")
	policy.HistoryBudget = &manifest.ImageHistoryBudget{
		RequestBytes: 14 * 1024 * 1024, RetryRequestBytes: 10 * 1024 * 1024,
		PerImageTargets: []int64{512 * 1024, 256 * 1024, 64 * 1024},
		OmitOldImages:   true, RetainNewestImage: true,
	}
	return policy
}

func integratedInstructions(profile, text string) *InstructionCapability {
	return &InstructionCapability{Default: profile, Profiles: map[string]string{profile: text}}
}

func cloneManifest(value manifest.Manifest) manifest.Manifest { return cloneJSON(value) }

func cloneJSON[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return value
	}
	return result
}

func ptr[T any](value T) *T { return &value }
