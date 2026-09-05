package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/mod/semver"
	"golang.org/x/net/http/httpguts"
)

const MaxManifestBytes = 1 << 20

var (
	pluginIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	providerIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	localIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	skillNamePattern    = regexp.MustCompile(`^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`)
	featureIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	usageContextPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	toolPrefixPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	hostnamePattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

// DecodeStrict decodes one bounded JSON manifest and rejects unknown fields or
// trailing JSON values. Opaque extension and embedded-schema objects remain open.
func DecodeStrict(data []byte) (Manifest, error) {
	if len(data) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value Manifest
	if err := decoder.Decode(&value); err != nil {
		return Manifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Manifest{}, err
	}
	if err := validateProviderSchema(data); err != nil {
		return Manifest{}, err
	}
	if err := Validate(value); err != nil {
		return Manifest{}, err
	}
	return value, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest contains multiple JSON values")
		}
		return fmt.Errorf("decoding trailing manifest data: %w", err)
	}
	return nil
}

// Validate applies semantic, reference, compatibility, and path rules that
// JSON Schema cannot express reliably.
func Validate(m Manifest) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if m.PluginType != "" && m.PluginType != PluginTypeProvider {
		add("plugin_type must be %q", PluginTypeProvider)
	}
	if m.ManifestVersion != Version {
		add("manifest_version must be %d", Version)
	}
	if !pluginIDPattern.MatchString(m.ID) || len(m.ID) > 128 {
		add("id is invalid")
	}
	if !validSemver(m.Version) {
		add("version must be canonical SemVer without a v prefix")
	}
	if strings.TrimSpace(m.Name) == "" {
		add("name is required")
	}
	if strings.TrimSpace(m.Description) == "" {
		add("description is required")
	}
	if !pluginIDPattern.MatchString(m.Publisher.ID) {
		add("publisher.id is invalid")
	}
	if m.Compatibility.HostAPI.Min < 1 || m.Compatibility.HostAPI.Max < m.Compatibility.HostAPI.Min {
		add("compatibility.host_api must be a nonempty ascending range")
	}
	if m.Compatibility.HostVersion != nil {
		if m.Compatibility.HostVersion.Min != "" && !validSemver(m.Compatibility.HostVersion.Min) {
			add("compatibility.host_version.min is invalid")
		}
		if m.Compatibility.HostVersion.Max != "" && !validSemver(m.Compatibility.HostVersion.Max) {
			add("compatibility.host_version.max is invalid")
		}
		if min, max := m.Compatibility.HostVersion.Min, m.Compatibility.HostVersion.Max; min != "" && max != "" && semver.Compare("v"+min, "v"+max) > 0 {
			add("compatibility.host_version min exceeds max")
		}
	}
	validateFeatureSets(m.Compatibility, add)
	if !providerIDPattern.MatchString(m.Provider.ID) || len(m.Provider.ID) > 64 {
		add("provider.id is invalid")
	}
	if strings.TrimSpace(m.Provider.Name) == "" {
		add("provider.name is required")
	}
	if !pluginIDPattern.MatchString(m.Provider.AccountNamespace) {
		add("provider.account_namespace is invalid")
	}
	if m.Provider.LoginOrder < 1 || m.Provider.AccountOrder < 1 {
		add("provider.login_order and provider.account_order must be explicit positive values")
	}
	validateProviderAliases(m.Provider, add)
	validateExtensions("extensions", m.Extensions, add)

	modelIDs := make(map[string]struct{}, len(m.Models))
	hasImageInput := false
	for i, model := range m.Models {
		prefix := fmt.Sprintf("models[%d]", i)
		if model.ID == "" {
			add("%s.id is required", prefix)
		}
		if _, exists := modelIDs[model.ID]; exists {
			add("%s.id %q is duplicated", prefix, model.ID)
		}
		modelIDs[model.ID] = struct{}{}
		if strings.TrimSpace(model.Name) == "" {
			add("%s.name is required", prefix)
		}
		if model.ContextWindow < 1 || model.DefaultMaxTokens < 1 {
			add("%s token limits must be positive", prefix)
		}
		if model.DefaultMaxTokens > model.ContextWindow {
			add("%s.default_max_tokens exceeds context_window", prefix)
		}
		if validateModelModalities(prefix, model.Modalities, add) {
			hasImageInput = true
		}
		if model.Reasoning != nil {
			if model.Reasoning.Default != "" && !slices.Contains(model.Reasoning.Levels, model.Reasoning.Default) {
				add("%s.reasoning.default is not in levels", prefix)
			}
			if model.Reasoning.Budgets != nil && model.Reasoning.Budgets.Max != 0 && model.Reasoning.Budgets.Min > model.Reasoning.Budgets.Max {
				add("%s.reasoning budget min exceeds max", prefix)
			}
		}
		validateExtensions(prefix+".extensions", model.Extensions, add)
	}
	for field, id := range map[string]string{"default_large_model": m.Provider.DefaultLargeModel, "default_small_model": m.Provider.DefaultSmallModel} {
		if _, ok := modelIDs[id]; !ok {
			add("provider.%s references unknown model %q", field, id)
		}
	}
	if hasImageInput && m.Capabilities.Images == nil {
		add("capabilities.images is required when a model declares image input")
	}
	if !hasImageInput && m.Capabilities.Images != nil {
		add("capabilities.images has no model declaring image input")
	}
	validateConfiguration(m.Configuration, add)
	validateCapabilities(m, add)
	return errors.Join(errs...)
}

func validateModelModalities(prefix string, modalities Modalities, add func(string, ...any)) bool {
	if len(modalities.Input) == 0 || len(modalities.Output) == 0 {
		add("%s.modalities input and output are required", prefix)
	}
	hasImageInput := false
	input := map[string]struct{}{}
	for _, modality := range modalities.Input {
		if _, exists := input[modality]; exists {
			add("%s.modalities.input contains duplicate modality %q", prefix, modality)
		}
		input[modality] = struct{}{}
		switch modality {
		case "text":
		case "image":
			hasImageInput = true
		default:
			add("%s.modalities.input %q is not executable by this host", prefix, modality)
		}
	}
	if len(modalities.Input) > 0 && !slices.Contains(modalities.Input, "text") {
		add("%s.modalities.input must include text", prefix)
	}
	output := map[string]struct{}{}
	for _, modality := range modalities.Output {
		if _, exists := output[modality]; exists {
			add("%s.modalities.output contains duplicate modality %q", prefix, modality)
		}
		output[modality] = struct{}{}
		if modality != "text" {
			add("%s.modalities.output %q is not executable by this host", prefix, modality)
		}
	}
	if len(modalities.Output) > 0 && !slices.Contains(modalities.Output, "text") {
		add("%s.modalities.output must include text", prefix)
	}
	return hasImageInput
}

func validSemver(value string) bool {
	return value != "" && !strings.HasPrefix(value, "v") && semver.IsValid("v"+value)
}

func validateFeatureSets(c Compatibility, add func(string, ...any)) {
	seen := map[string]string{}
	for label, values := range map[string][]string{"required_features": c.RequiredFeatures, "optional_features": c.OptionalFeatures} {
		for _, value := range values {
			if !featureIDPattern.MatchString(value) {
				add("compatibility.%s contains invalid feature %q", label, value)
			}
			if previous, ok := seen[value]; ok {
				add("compatibility feature %q appears in both %s and %s", value, previous, label)
			} else {
				seen[value] = label
			}
		}
	}
}

func validateExtensions(prefix string, values map[string]json.RawMessage, add func(string, ...any)) {
	for key, value := range values {
		if !strings.HasPrefix(key, "x-") || !pluginIDPattern.MatchString(strings.TrimPrefix(key, "x-")) {
			add("%s key %q must be a namespaced x-* identifier", prefix, key)
		}
		if !json.Valid(value) {
			add("%s.%s is not valid JSON", prefix, key)
		}
	}
}

func validateConfiguration(c Configuration, add func(string, ...any)) {
	if c.Schema == nil {
		add("configuration.schema is required")
		return
	}
	if c.Schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		add("configuration.schema must declare Draft 2020-12")
	}
	if c.Schema["type"] != "object" {
		add("configuration.schema type must be object")
	}
	if c.Schema["additionalProperties"] != false {
		add("configuration.schema must set additionalProperties to false")
	}
	properties, _ := c.Schema["properties"].(map[string]any)
	for name := range c.Fields {
		if _, ok := properties[name]; !ok {
			add("configuration.fields.%s has no matching schema property", name)
		}
	}
}

func collectConfigurationRefs(c Configuration) map[string]struct{} {
	result := map[string]struct{}{}
	properties, _ := c.Schema["properties"].(map[string]any)
	for name := range properties {
		result[name] = struct{}{}
	}
	return result
}

func validateCapabilities(m Manifest, add func(string, ...any)) {
	c := m.Capabilities
	validateCompatibilityAdapter(c.Compatibility, add)
	configProperties := collectConfigurationRefs(m.Configuration)
	credentials := collectIDs("credentials", len(c.Credentials), func(i int) string { return c.Credentials[i].ID }, add)
	clientIdentities := make(map[string]struct{}, len(c.ClientIdentities))
	if len(c.ClientIdentities) > 32 {
		add("client_identities exceeds 32 entries")
	}
	for name, identity := range c.ClientIdentities {
		if !localIDPattern.MatchString(name) || len(name) > 64 {
			add("client_identities key %q is invalid", name)
		}
		clientIdentities[name] = struct{}{}
		validateClientIdentity("client_identities."+name, &identity, add)
	}
	endpoints := collectIDs("endpoints", len(c.Endpoints), func(i int) string { return c.Endpoints[i].ID }, add)
	oauth := collectIDs("oauth", len(c.OAuth), func(i int) string { return c.OAuth[i].ID }, add)
	operations := collectIDs("operations", len(c.Operations), func(i int) string { return c.Operations[i].ID }, add)
	_ = oauth

	credentialValues := make(map[string]Credential, len(c.Credentials))
	for i, credential := range c.Credentials {
		credentialValues[credential.ID] = credential
		for _, endpoint := range credential.Audience {
			requireRef(fmt.Sprintf("credentials[%d].audience", i), endpoint, endpoints, add)
		}
		if credential.ConfigProperty != "" {
			if _, ok := configProperties[credential.ConfigProperty]; !ok {
				add("credentials[%d].config_property references unknown configuration property %q", i, credential.ConfigProperty)
			}
		}
	}
	for i, endpoint := range c.Endpoints {
		validateEndpoint(i, endpoint, credentials, add)
		if endpoint.Credential != "" {
			credential := credentialValues[endpoint.Credential]
			if !slices.Contains(credential.Audience, endpoint.ID) {
				add("endpoints[%d].credential %q does not declare endpoint %q in its audience", i, endpoint.Credential, endpoint.ID)
			}
		}
	}
	for i, flow := range c.OAuth {
		prefix := fmt.Sprintf("oauth[%d]", i)
		requireRef(prefix+".credential", flow.Credential, credentials, add)
		credential := credentialValues[flow.Credential]
		for _, scope := range append(slices.Clone(flow.Scopes), flow.RefreshScopes...) {
			if !slices.Contains(credential.Scopes, scope) {
				add("%s scope %q exceeds credential scope declaration", prefix, scope)
			}
		}
		requireRef(prefix+".authorization_endpoint", flow.AuthorizationEndpoint, endpoints, add)
		requireRef(prefix+".token_endpoint", flow.TokenEndpoint, endpoints, add)
		if flow.RevocationEndpoint != "" {
			requireRef(prefix+".revocation_endpoint", flow.RevocationEndpoint, endpoints, add)
			add("%s.revocation_endpoint is unsupported by this host", prefix)
		}
		if flow.TimeoutSeconds == 0 {
			add("%s.timeout_seconds must be explicit", prefix)
		}
		if flow.TokenResponse.MaxBodyBytes == 0 {
			add("%s.token_response.max_body_bytes must be explicit", prefix)
		}
		validateTemplate(prefix+".client_id", flow.ClientID, credentials, configProperties, add)
		if flow.ClientSecret != nil {
			validateTemplate(prefix+".client_secret", *flow.ClientSecret, credentials, configProperties, add)
		}
		for j, rule := range flow.AuthorizationParams {
			validateTemplate(fmt.Sprintf("%s.authorization_params[%d].value", prefix, j), rule.Value, credentials, configProperties, add)
		}
		for label, rules := range map[string][]FieldRule{"authorization_code": flow.TokenRequest.Code, "refresh_token": flow.TokenRequest.Refresh} {
			for j, rule := range rules {
				validateTemplate(fmt.Sprintf("%s.token_request.%s[%d].value", prefix, label, j), rule.Value, credentials, configProperties, add)
			}
		}
		validateHeaderRules(prefix+".token_request.headers", flow.TokenRequest.Headers, credentials, configProperties, add)
		if flow.Redirect.Mode == "device-code" && flow.DeviceCode == nil {
			add("%s.device_code is required for device-code redirect mode", prefix)
		}
		if flow.Redirect.Mode != "device-code" && flow.DeviceCode != nil {
			add("%s.device_code is only valid for device-code redirect mode", prefix)
		}
		if flow.DeviceCode != nil {
			requireRef(prefix+".device_code.endpoint", flow.DeviceCode.Endpoint, endpoints, add)
			for j, rule := range flow.DeviceCode.Request {
				validateTemplate(fmt.Sprintf("%s.device_code.request[%d].value", prefix, j), rule.Value, credentials, configProperties, add)
			}
			for j, rule := range flow.DeviceCode.Poll {
				validateTemplate(fmt.Sprintf("%s.device_code.poll[%d].value", prefix, j), rule.Value, credentials, configProperties, add)
			}
			validateHeaderRules(prefix+".device_code.headers", flow.DeviceCode.Headers, credentials, configProperties, add)
		}
	}
	validateHeaderRules("headers", c.Headers, credentials, configProperties, add)
	for name, pipeline := range c.JSONTransforms {
		if pipeline.MaxOperations == 0 {
			add("json_transforms.%s.max_operations must be explicit", name)
		}
		if len(pipeline.Operations) > pipeline.MaxOperations {
			add("json_transforms.%s exceeds max_operations", name)
		}
		for i, operation := range pipeline.Operations {
			prefix := fmt.Sprintf("json_transforms.%s.operations[%d]", name, i)
			if operation.Value != nil {
				validateTemplate(prefix+".value", *operation.Value, credentials, configProperties, add)
			}
			validatePredicate(prefix+".predicate", operation.Predicate, credentials, configProperties, add)
		}
	}
	for name, pipeline := range c.PromptTransforms {
		for i, operation := range pipeline.Operations {
			prefix := fmt.Sprintf("prompt_transforms.%s.operations[%d]", name, i)
			if operation.Text != nil {
				validateTemplate(prefix+".text", *operation.Text, credentials, configProperties, add)
			}
			validatePredicate(prefix+".when", operation.When, credentials, configProperties, add)
		}
	}
	for name, codec := range c.ToolCodecs {
		validateToolCodec(name, codec, add)
	}
	usageOperations := map[string]struct{}{}
	if c.Usage != nil && c.Usage.Source == "operation" {
		usageOperations[c.Usage.Operation] = struct{}{}
		for _, setup := range c.Usage.Setup {
			usageOperations[setup.Operation] = struct{}{}
		}
	}
	identityReferences := make(map[string]int, len(c.ClientIdentities))
	inferenceOperations := 0
	for i, operation := range c.Operations {
		validateOperation(i, operation, endpoints, operations, clientIdentities, usageOperations, c, configProperties, add)
		if operation.ClientIdentity != "" {
			identityReferences[operation.ClientIdentity]++
		}
		if operation.Kind == "inference" {
			inferenceOperations++
		}
	}
	for name := range c.ClientIdentities {
		if identityReferences[name] == 0 {
			add("client_identities.%s is not referenced by an operation", name)
		}
	}
	if inferenceOperations != 1 {
		add("capabilities.operations must declare exactly one inference operation")
	}
	validateAnthropicPolicy(c.Anthropic, c.Instructions, c.Operations, add)
	validateUsagePolicy(c.Usage, c.Operations, c.Endpoints, operations, c.JSONTransforms, c.Anthropic != nil && c.Anthropic.ClientIdentity != nil, add)
	if c.Compatibility == nil && c.Usage != nil && c.Usage.Source == "operation" && len(c.Usage.Mappings) > 0 {
		add("usage.mappings are unavailable for source operation")
	}
	if c.Instructions != nil {
		if c.Instructions.SelectionDefault != "" && c.Instructions.SelectionDefault != "crux" && c.Instructions.SelectionDefault != "native" {
			add("instructions.selection_default must be crux or native")
		}
		if _, ok := c.Instructions.Profiles[c.Instructions.Default]; !ok {
			add("instructions.default references unknown profile")
		}
		for id, file := range c.Instructions.Profiles {
			if !safeBundlePath(file) {
				add("instructions.profiles.%s is not a safe bundle-relative path", id)
			}
		}
		hiddenSkills := make(map[string]struct{}, len(c.Instructions.HiddenSkills))
		for i, name := range c.Instructions.HiddenSkills {
			if len(name) > 64 || !skillNamePattern.MatchString(name) {
				add("instructions.hidden_skills[%d] is not a valid skill name", i)
			}
			if _, exists := hiddenSkills[name]; exists {
				add("instructions.hidden_skills duplicates %q", name)
			}
			hiddenSkills[name] = struct{}{}
		}
	}
	validateImagePolicy(c.Images, add)
	validateRuntimeControls(c.RuntimeControls, add)
	validateErrorMappings(c.Errors, add)
	metadata := map[string]struct{}{}
	for i, contract := range c.Metadata {
		key := contract.Namespace
		if c.Compatibility != nil {
			key += "\x00" + contract.Scope
		}
		if _, ok := metadata[key]; ok {
			add("metadata[%d] duplicates namespace %q", i, contract.Namespace)
		}
		metadata[key] = struct{}{}
		if contract.Schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			add("metadata[%d].schema must declare Draft 2020-12", i)
		}
		if _, err := CompileMetadataContracts([]MetadataContract{contract}); err != nil {
			add("metadata[%d]: %v", i, err)
		}
	}
}

func validateUsagePolicy(policy *UsagePolicy, declarations []Operation, endpoints []Endpoint, operations map[string]struct{}, transforms map[string]JSONPipeline, anthropicIdentity bool, add func(string, ...any)) {
	if policy == nil {
		return
	}
	if err := ValidateUsageMappings(policy); err != nil {
		add("%v", err)
	}
	for i, pointer := range policy.PlanPointers {
		validateUsagePointer(fmt.Sprintf("usage.plan_pointers[%d]", i), pointer, add)
	}
	windowIDs := make(map[string]struct{}, len(policy.Windows))
	for i, window := range policy.Windows {
		validateUsageWindow(i, window, add)
		if _, exists := windowIDs[window.ID]; exists {
			add("usage.windows[%d].id %q is duplicated", i, window.ID)
		}
		windowIDs[window.ID] = struct{}{}
	}
	if policy.Source != "operation" {
		if len(policy.Setup) > 0 {
			add("usage.setup requires source operation")
		}
		if len(policy.PlanPointers) > 0 {
			add("usage.plan_pointers requires source operation")
		}
		return
	}
	if policy.Operation == "" {
		add("usage.operation is required for source operation")
		return
	}
	requireRef("usage.operation", policy.Operation, operations, add)
	if policy.Fallback != "unavailable" {
		add("usage fallback %q is unsupported for source operation", policy.Fallback)
	}
	byID := make(map[string]Operation, len(declarations))
	for _, operation := range declarations {
		byID[operation.ID] = operation
	}
	endpointCredentials := make(map[string]string, len(endpoints))
	for _, endpoint := range endpoints {
		endpointCredentials[endpoint.ID] = endpoint.Credential
	}
	available := map[string]struct{}{}
	for i, setup := range policy.Setup {
		prefix := fmt.Sprintf("usage.setup[%d]", i)
		requireRef(prefix+".operation", setup.Operation, operations, add)
		if operation, ok := byID[setup.Operation]; ok {
			validateUsageOperation(prefix+".operation", operation, transforms, usageAvailableContext(available, operation, anthropicIdentity), endpointCredentials[operation.Endpoint], false, add)
		}
		for j, pointer := range setup.PlanPointers {
			validateUsagePointer(fmt.Sprintf("%s.plan_pointers[%d]", prefix, j), pointer, add)
		}
		for j, extraction := range setup.Extract {
			field := fmt.Sprintf("%s.extract[%d]", prefix, j)
			if !usageContextPattern.MatchString(extraction.Context) || len(extraction.Context) > 128 {
				add("%s.context is invalid", field)
			}
			if _, exists := available[extraction.Context]; exists {
				add("%s.context %q is already defined", field, extraction.Context)
			}
			validateUsagePointer(field+".pointer", extraction.Pointer, add)
			available[extraction.Context] = struct{}{}
		}
	}
	if operation, ok := byID[policy.Operation]; ok {
		validateUsageOperation("usage.operation", operation, transforms, usageAvailableContext(available, operation, anthropicIdentity), endpointCredentials[operation.Endpoint], true, add)
	}
}

func validateUsageWindow(index int, window WindowMap, add func(string, ...any)) {
	prefix := fmt.Sprintf("usage.windows[%d]", index)
	if strings.TrimSpace(window.ID) == "" || len(window.ID) > 64 {
		add("%s.id is invalid", prefix)
	}
	for name, pointer := range map[string]string{
		"used_pointer": window.UsedPointer, "limit_pointer": window.LimitPointer,
		"remaining_pointer": window.RemainingPointer, "remaining_fraction_pointer": window.RemainingFractionPointer,
		"reset_pointer": window.ResetPointer,
	} {
		if pointer != "" {
			validateUsagePointer(prefix+"."+name, pointer, add)
		}
	}
	sources := 0
	if window.UsedPointer != "" {
		sources++
	}
	if window.RemainingPointer != "" {
		sources++
	}
	if window.RemainingFractionPointer != "" {
		sources++
	}
	if sources != 1 {
		add("%s must declare exactly one used, remaining, or remaining-fraction pointer", prefix)
	}
	if window.RemainingPointer != "" && window.LimitPointer == "" {
		add("%s.limit_pointer is required with remaining_pointer", prefix)
	}
	if window.RemainingFractionPointer != "" && window.LimitPointer != "" {
		add("%s.limit_pointer cannot be combined with remaining_fraction_pointer", prefix)
	}
	if window.LimitPointer != "" && window.UsedPointer == "" && window.RemainingPointer == "" {
		add("%s.limit_pointer requires used_pointer or remaining_pointer", prefix)
	}
	if (window.ResetPointer == "") != (window.ResetFormat == "") {
		add("%s.reset_pointer and reset_format must be declared together", prefix)
	}
}

func validateUsagePointer(field, pointer string, add func(string, ...any)) {
	if pointer == "" || len(pointer) > 1024 || !ValidJSONPointer(pointer) {
		add("%s is not a valid non-root JSON Pointer", field)
	}
}

func validateUsageOperation(prefix string, operation Operation, transforms map[string]JSONPipeline, available map[string]struct{}, endpointCredential string, final bool, add func(string, ...any)) {
	if final {
		if operation.Kind != "usage" {
			add("%s must reference a usage operation", prefix)
		}
	} else if operation.Kind != "account" && operation.Kind != "custom" && operation.Kind != "usage" {
		add("%s references unsupported setup operation kind %q", prefix, operation.Kind)
	}
	if operation.Protocol != "generic-json" || operation.Transport != "http-json" {
		add("%s requires generic-json over http-json", prefix)
	}
	if operation.Streaming != nil || operation.PromptTransform != "" || operation.RoleMap != "" || operation.ToolCodec != "" || operation.Continuation != nil || operation.Compaction != nil {
		add("%s declares policy unavailable to operation-sourced usage", prefix)
	}
	if retry := operation.Retry; retry != nil {
		if retry.MaxAttempts != 1 || len(retry.Statuses) > 0 || len(retry.Codes) > 0 || retry.TransportErrors || retry.UnexpectedEOF || retry.Authentication != "never" || retry.ReplayRequirement != "never" {
			add("%s retry policy is unavailable to operation-sourced usage", prefix)
		}
	}
	for i, rule := range operation.Headers {
		if rule.Value != nil {
			validateUsageTemplate(fmt.Sprintf("%s.headers[%d].value", prefix, i), *rule.Value, available, endpointCredential, add)
		}
	}
	for label, name := range map[string]string{"request_transform": operation.RequestTransform, "response_transform": operation.ResponseTransform} {
		if name == "" {
			continue
		}
		pipeline, ok := transforms[name]
		if !ok {
			continue
		}
		for i, item := range pipeline.Operations {
			field := fmt.Sprintf("%s.%s.operations[%d]", prefix, label, i)
			if item.Value != nil {
				validateUsageTemplate(field+".value", *item.Value, available, endpointCredential, add)
			}
			if item.Predicate != nil {
				if item.Predicate.Value != nil {
					validateUsageTemplate(field+".predicate.value", *item.Predicate.Value, available, endpointCredential, add)
				}
				for j, value := range item.Predicate.Values {
					validateUsageTemplate(fmt.Sprintf("%s.predicate.values[%d]", field, j), value, available, endpointCredential, add)
				}
			}
		}
	}
}

func validateUsageTemplate(prefix string, value Template, available map[string]struct{}, credential string, add func(string, ...any)) {
	switch value.Kind {
	case "context":
		if _, ok := available[value.Ref]; !ok {
			add("%s references unavailable usage context %q", prefix, value.Ref)
		}
	case "config":
		add("%s configuration templates are unavailable to operation-sourced usage", prefix)
	case "credential":
		if credential == "" || value.Ref != credential {
			add("%s credential %q does not match the operation endpoint credential", prefix, value.Ref)
		}
	}
	for i, part := range value.Parts {
		validateUsageTemplate(fmt.Sprintf("%s.parts[%d]", prefix, i), part, available, credential, add)
	}
}

func validateProviderAliases(provider Provider, add func(string, ...any)) {
	seen := map[string]struct{}{strings.ToLower(provider.ID): {}}
	for _, alias := range provider.Aliases {
		canonical := strings.ToLower(strings.TrimSpace(alias))
		if !providerIDPattern.MatchString(canonical) || len(canonical) > 64 {
			add("provider.aliases contains invalid alias %q", alias)
			continue
		}
		if _, ok := seen[canonical]; ok {
			add("provider.aliases contains duplicate alias %q", alias)
		}
		seen[canonical] = struct{}{}
	}
}

func validateCompatibilityAdapter(adapter *CompatibilityAdapter, add func(string, ...any)) {
	if adapter == nil {
		return
	}
	if !strings.HasPrefix(adapter.ID, "integrated-") || !pluginIDPattern.MatchString(strings.TrimPrefix(adapter.ID, "integrated-")) {
		add("compatibility_adapter.id must identify a host-known integrated adapter")
	}
	if !slices.Contains(adapter.Delegates, "construction") {
		add("compatibility_adapter.delegates must include construction")
	}
	covered := make(map[string]bool, len(adapter.Delegates))
	for i, item := range adapter.Inventory {
		if !slices.Contains(adapter.Delegates, item.Delegate) {
			add("compatibility_adapter.inventory[%d].delegate %q is not delegated", i, item.Delegate)
		} else {
			covered[item.Delegate] = true
		}
		if strings.TrimSpace(item.Behavior) == "" {
			add("compatibility_adapter.inventory[%d].behavior must be non-empty", i)
		}
		if item.Classification == "finite-core-primitive" && strings.TrimSpace(item.Primitive) == "" {
			add("compatibility_adapter.inventory[%d].primitive must name the proposed finite core primitive", i)
		}
		if item.Classification == "private-stateful" && strings.TrimSpace(item.Primitive) != "" {
			add("compatibility_adapter.inventory[%d].primitive must be empty for private-stateful behavior", i)
		}
	}
	for _, delegate := range adapter.Delegates {
		if !covered[delegate] {
			add("compatibility_adapter.inventory must classify delegated capability %q", delegate)
		}
	}
}

// validateAnthropicPolicy is the admission gate for the manifest-backed
// Anthropic transport. It binds policy to the correct protocol and validates
// request bounds, header deletion, prompt transforms, client version and
// User-Agent resolution, and billing attribution before any private values can
// reach runtime. Do not weaken these checks or move identity handling to an
// unvalidated generic transport.
func usageAvailableContext(available map[string]struct{}, operation Operation, anthropicIdentity bool) map[string]struct{} {
	result := make(map[string]struct{}, len(available)+1)
	for name := range available {
		result[name] = struct{}{}
	}
	if operation.ClientIdentity != "" || anthropicIdentity {
		result["client.user_agent"] = struct{}{}
	}
	return result
}

func validateClientIdentity(prefix string, identity *ResolvedClientIdentity, add func(string, ...any)) {
	if identity == nil {
		return
	}
	if identity.Environment != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(identity.Environment) {
		add("%s.environment is invalid", prefix)
	}
	if !localIDPattern.MatchString(identity.CacheKey) || len(identity.CacheKey) > 64 {
		add("%s.cache_key is invalid", prefix)
	}
	if identity.LatestURL != "" {
		if !validClientIdentityTemplate(identity.LatestURL, false) {
			add("%s.latest_url contains an unsupported placeholder", prefix)
		}
		candidate := strings.NewReplacer("{os}", "darwin", "{arch}", "arm64").Replace(identity.LatestURL)
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			add("%s.latest_url must be an absolute credential-free HTTPS URL without fragment", prefix)
		}
	} else if identity.VersionPointer != "" {
		add("%s.version_pointer requires latest_url", prefix)
	}
	if identity.VersionPointer != "" && (len(identity.VersionPointer) > 1024 || !ValidJSONPointer(identity.VersionPointer)) {
		add("%s.version_pointer is not a valid non-root JSON Pointer", prefix)
	}
	pattern, err := regexp.Compile(identity.VersionPattern)
	if err != nil {
		add("%s.version_pattern is invalid", prefix)
	} else if !pattern.MatchString(identity.FallbackVersion) {
		add("%s.fallback_version does not match version_pattern", prefix)
	}
	if strings.Count(identity.UserAgentFormat, "{version}") != 1 {
		add("%s.user_agent_format must contain {version} exactly once", prefix)
	}
	if !validClientIdentityTemplate(identity.UserAgentFormat, true) {
		add("%s.user_agent_format contains an unsupported placeholder", prefix)
	}
	if identity.ProbeTimeoutMS < 1 || identity.ProbeTimeoutMS > 30000 {
		add("%s.probe_timeout_ms must be between 1 and 30000", prefix)
	}
	if identity.ProbeMaxBytes < 1 || identity.ProbeMaxBytes > 65536 {
		add("%s.probe_max_bytes must be between 1 and 65536", prefix)
	}
}

func validClientIdentityTemplate(value string, allowVersion bool) bool {
	value = strings.NewReplacer("{os}", "", "{arch}", "").Replace(value)
	if allowVersion {
		value = strings.ReplaceAll(value, "{version}", "")
	}
	return !strings.ContainsAny(value, "{}")
}

func validateAnthropicPolicy(policy *AnthropicPolicy, instructions *InstructionPolicy, operations []Operation, add func(string, ...any)) {
	if policy == nil {
		return
	}
	bound := false
	for _, operation := range operations {
		if operation.Kind == "inference" && operation.Protocol == "anthropic-messages" {
			bound = true
			break
		}
	}
	if !bound {
		add("anthropic policy requires an anthropic-messages inference operation")
	}
	if policy.MaxRequestBytes <= 0 {
		add("anthropic.max_request_bytes must be explicit")
	}
	if policy.SessionHeader != "" && !HeaderNameValid(policy.SessionHeader) {
		add("anthropic.session_header is not a valid HTTP header name")
	}
	for i, name := range policy.DeleteHeaders {
		if !HeaderNameValid(name) {
			add("anthropic.delete_headers[%d] is not a valid HTTP header name", i)
		}
	}
	for i, prefix := range policy.DeleteHeaderPrefixes {
		if strings.TrimSpace(prefix) == "" || strings.ContainsAny(prefix, "\r\n:") {
			add("anthropic.delete_header_prefixes[%d] is invalid", i)
		}
	}
	for i, prefix := range policy.SystemLinePrefixes {
		if strings.TrimSpace(prefix) == "" || strings.ContainsAny(prefix, "\r\n") {
			add("anthropic.system_line_prefixes[%d] is invalid", i)
		}
	}
	for i, prefix := range policy.SystemPrefixes {
		if strings.TrimSpace(prefix) == "" {
			add("anthropic.system_prefixes[%d] must be non-empty", i)
		}
	}
	for i, block := range policy.SystemBlocks {
		if strings.TrimSpace(block.Text) == "" {
			add("anthropic.system_blocks[%d].text must be non-empty", i)
		}
		validateAnthropicCacheControl(fmt.Sprintf("anthropic.system_blocks[%d].cache_control", i), block.CacheControl, add)
	}
	if block := policy.InstructionBlock; block != nil {
		if instructions == nil {
			add("anthropic.instruction_block requires instructions")
		} else if _, ok := instructions.Profiles[block.Profile]; !ok {
			add("anthropic.instruction_block.profile references unknown instruction profile %q", block.Profile)
		}
		validateAnthropicCacheControl("anthropic.instruction_block.cache_control", block.CacheControl, add)
	}
	validateClientIdentity("anthropic.client_identity", policy.ClientIdentity, add)
	if billing := policy.Billing; billing != nil {
		seen := map[int]struct{}{}
		for i, offset := range billing.ByteOffsets {
			if offset < 0 {
				add("anthropic.billing.byte_offsets[%d] must be non-negative", i)
			}
			if _, ok := seen[offset]; ok {
				add("anthropic.billing.byte_offsets[%d] is duplicated", i)
			}
			seen[offset] = struct{}{}
		}
		if len([]byte(billing.MissingByte)) != 1 {
			add("anthropic.billing.missing_byte must be exactly one byte")
		}
		if strings.Count(billing.Format, "{version}") != 1 || strings.Count(billing.Format, "{fingerprint}") > 1 {
			add("anthropic.billing.format must contain {version} exactly once and {fingerprint} at most once")
		}
	}
}

func validateAnthropicCacheControl(prefix string, cache *AnthropicCacheControl, add func(string, ...any)) {
	if cache == nil {
		return
	}
	if cache.Type != "ephemeral" {
		add("%s.type must be ephemeral", prefix)
	}
	if cache.TTL != "" && cache.TTL != "5m" && cache.TTL != "1h" {
		add("%s.ttl must be 5m or 1h", prefix)
	}
	if cache.Scope != "" && cache.Scope != "global" {
		add("%s.scope must be global", prefix)
	}
}

func validateImagePolicy(policy *ImagePolicy, add func(string, ...any)) {
	if policy == nil {
		return
	}
	if err := ValidateImagePolicyExecution(policy); err != nil {
		add("%v", err)
	}
	if len(policy.AcceptedMediaTypes) == 0 {
		add("images.accepted_media_types must be non-empty")
	}
	if len(policy.AcceptedMediaTypes) > 32 {
		add("images.accepted_media_types exceeds 32 entries")
	}
	mediaTypes := map[string]struct{}{}
	for _, mediaType := range policy.AcceptedMediaTypes {
		if _, exists := mediaTypes[mediaType]; exists {
			add("images.accepted_media_types contains duplicate media type %q", mediaType)
		}
		mediaTypes[mediaType] = struct{}{}
		if !slices.Contains([]string{"image/gif", "image/jpeg", "image/png", "image/webp"}, mediaType) {
			add("images.accepted_media_types contains unsupported media type %q", mediaType)
		}
	}
	if policy.MaxSourceBytes < 1 || policy.MaxSourceBytes > 100*1024*1024 {
		add("images.max_source_bytes must be between 1 and 104857600")
	}
	if policy.MaxSidePixels < 0 || policy.MaxSidePixels > 32768 {
		add("images.max_side_pixels must be between 1 and 32768 when declared")
	}
	if policy.MaxOutputBytes < 0 || policy.MaxOutputBytes > 100*1024*1024 {
		add("images.max_output_bytes must be between 1 and 104857600 when declared")
	}
	if policy.MaxPatches < 0 {
		add("images.max_patches must be positive when declared")
	}
	if policy.OutputMediaType != "" && policy.OutputMediaType != "image/jpeg" && policy.OutputMediaType != "image/png" {
		add("images.output_media_type %q is not supported by the core image encoder", policy.OutputMediaType)
	} else if policy.OutputMediaType != "" && !slices.Contains(policy.AcceptedMediaTypes, policy.OutputMediaType) {
		add("images.output_media_type %q is not declared in accepted_media_types", policy.OutputMediaType)
	}
	if policy.FlattenAlpha != "" && policy.FlattenAlpha != "none" && policy.FlattenAlpha != "white" && policy.FlattenAlpha != "black" {
		add("images.flatten_alpha must be none, white, or black")
	}
	if len(policy.QualitySteps) > 32 {
		add("images.quality_steps exceeds 32 entries")
	}
	qualities := map[int]struct{}{}
	for _, quality := range policy.QualitySteps {
		if _, exists := qualities[quality]; exists {
			add("images.quality_steps contains duplicate JPEG quality %d", quality)
		}
		qualities[quality] = struct{}{}
		if quality < 1 || quality > 100 {
			add("images.quality_steps contains invalid JPEG quality %d", quality)
		}
	}
	if len(policy.QualitySteps) > 0 {
		hasImageTransform := policy.MaxSidePixels > 0 || policy.MaxOutputBytes > 0 || policy.MaxPatches > 0 || policy.OutputMediaType != "" || policy.FlattenAlpha == "white" || policy.FlattenAlpha == "black"
		if !hasImageTransform {
			add("images.quality_steps has no executable image transformation")
		}
		if policy.OutputMediaType == "image/png" || !slices.Contains(policy.AcceptedMediaTypes, "image/jpeg") {
			add("images.quality_steps requires executable JPEG output")
		}
	}
	if policy.ResizePercent < 0 || policy.ResizePercent > 99 {
		add("images.resize_percent must be between 1 and 99 when declared")
	} else if policy.ResizePercent > 0 && policy.MaxOutputBytes == 0 {
		add("images.resize_percent requires max_output_bytes")
	}
	if history := policy.HistoryBudget; history != nil {
		if history.RequestBytes < 1 {
			add("images.history_budget.request_bytes must be positive")
		}
		if history.RetryRequestBytes < 0 {
			add("images.history_budget.retry_request_bytes must be positive when declared")
		}
		if len(history.PerImageTargets) > 16 {
			add("images.history_budget.per_image_targets exceeds 16 entries")
		}
		targets := map[int64]struct{}{}
		for _, target := range history.PerImageTargets {
			if _, exists := targets[target]; exists {
				add("images.history_budget.per_image_targets contains duplicate target %d", target)
			}
			targets[target] = struct{}{}
			if target < 1 {
				add("images.history_budget.per_image_targets contains non-positive target %d", target)
			}
		}
		if history.RetainNewestImage && !history.OmitOldImages {
			add("images.history_budget.retain_newest_image requires omit_old_images")
		}
	}
}

func ValidateImagePolicyExecution(policy *ImagePolicy) error {
	if policy == nil {
		return nil
	}
	if policy.OutputMediaType != "" {
		if policy.OutputMediaType != "image/jpeg" && policy.OutputMediaType != "image/png" {
			return fmt.Errorf("images.output_media_type %q is not supported by the core image encoder", policy.OutputMediaType)
		}
		if !slices.Contains(policy.AcceptedMediaTypes, policy.OutputMediaType) {
			return fmt.Errorf("images.output_media_type %q is not declared in accepted_media_types", policy.OutputMediaType)
		}
		return nil
	}
	requiresEncoding := policy.MaxSidePixels > 0 || policy.MaxOutputBytes > 0 || policy.MaxPatches > 0 || policy.FlattenAlpha == "white" || policy.FlattenAlpha == "black"
	if requiresEncoding && !slices.Contains(policy.AcceptedMediaTypes, "image/jpeg") && !slices.Contains(policy.AcceptedMediaTypes, "image/png") {
		return fmt.Errorf("images transformation has no accepted output media type supported by the core image encoder")
	}
	return nil
}

func validateRuntimeControls(controls []RuntimeControl, add func(string, ...any)) {
	ids := map[string]struct{}{}
	for i, control := range controls {
		prefix := fmt.Sprintf("runtime_controls[%d]", i)
		if _, ok := ids[control.ID]; ok {
			add("%s.id %q is duplicated", prefix, control.ID)
		}
		ids[control.ID] = struct{}{}
		if control.RequestPath != "" && !ValidJSONPointer(control.RequestPath) {
			add("%s.request_path is not a valid JSON Pointer", prefix)
		}
		if control.Type == "enum" {
			if len(control.Values) == 0 {
				add("%s.values must be non-empty for enum controls", prefix)
			}
			if control.Default != nil {
				value, ok := control.Default.(string)
				if !ok || !slices.Contains(control.Values, value) {
					add("%s.default must be one of values", prefix)
				}
			}
		} else {
			if len(control.Values) != 0 {
				add("%s.values are only valid for enum controls", prefix)
			}
			if control.Default != nil && !runtimeDefaultMatches(control.Type, control.Default) {
				add("%s.default does not match type %q", prefix, control.Type)
			}
		}
	}
}

func runtimeDefaultMatches(kind string, value any) bool {
	switch kind {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		switch value := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return !math.IsInf(value, 0) && !math.IsNaN(value) && math.Trunc(value) == value
		case json.Number:
			_, err := value.Int64()
			return err == nil
		}
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		}
	}
	return false
}

func ValidateErrorMappings(mappings []ErrorMapping) error {
	var errs []error
	validateErrorMappings(mappings, func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	})
	return errors.Join(errs...)
}

func validateErrorMappings(mappings []ErrorMapping, add func(string, ...any)) {
	for i, mapping := range mappings {
		prefix := fmt.Sprintf("errors[%d]", i)
		for _, status := range mapping.Statuses {
			if status < 100 || status > 599 {
				add("%s.statuses contains invalid HTTP status %d", prefix, status)
			}
		}
		for j, code := range mapping.Codes {
			if code == "" {
				add("%s.codes[%d] must not be empty", prefix, j)
			}
			if utf8.RuneCountInString(code) > 256 {
				add("%s.codes[%d] exceeds 256 characters", prefix, j)
			}
		}
		if len(mapping.Codes) > 0 && mapping.CodePointer == "" {
			add("%s.code_pointer is required when codes are declared", prefix)
		}
		if len(mapping.Codes) == 0 && mapping.CodePointer != "" {
			add("%s.code_pointer has no codes to match", prefix)
		}
		if utf8.RuneCountInString(mapping.CodePointer) > 1024 {
			add("%s.code_pointer exceeds 1024 characters", prefix)
		} else if mapping.CodePointer != "" && !ValidJSONPointer(mapping.CodePointer) {
			add("%s.code_pointer is not a valid JSON Pointer", prefix)
		}
		if utf8.RuneCountInString(mapping.MessagePointer) > 1024 {
			add("%s.message_pointer exceeds 1024 characters", prefix)
		} else if mapping.MessagePointer != "" && !ValidJSONPointer(mapping.MessagePointer) {
			add("%s.message_pointer is not a valid JSON Pointer", prefix)
		}
		for earlier := range i {
			if errorMappingShadows(mappings[earlier], mapping) {
				add("%s is unreachable because errors[%d] matches every error it could match", prefix, earlier)
				break
			}
		}
	}
}

func errorMappingShadows(earlier, later ErrorMapping) bool {
	return errorStatusPredicateContains(earlier.Statuses, later.Statuses) &&
		errorCodePredicateContains(earlier, later)
}

func errorStatusPredicateContains(earlier, later []int) bool {
	if len(earlier) == 0 {
		return true
	}
	if len(later) == 0 {
		return false
	}
	for _, status := range later {
		if !slices.Contains(earlier, status) {
			return false
		}
	}
	return true
}

func errorCodePredicateContains(earlier, later ErrorMapping) bool {
	if len(earlier.Codes) == 0 {
		return true
	}
	if len(later.Codes) == 0 || earlier.CodePointer != later.CodePointer {
		return false
	}
	for _, code := range later.Codes {
		if !slices.Contains(earlier.Codes, code) {
			return false
		}
	}
	return true
}

func ValidJSONPointer(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] != '~' {
			continue
		}
		if i+1 >= len(value) || (value[i+1] != '0' && value[i+1] != '1') {
			return false
		}
		i++
	}
	return true
}

var normalizedEventFields = map[string]map[string]struct{}{
	"warning":          {"message": {}},
	"text-start":       {"id": {}},
	"text-delta":       {"id": {}, "delta": {}},
	"text-end":         {"id": {}},
	"reasoning-start":  {"id": {}, "delta": {}},
	"reasoning-delta":  {"id": {}, "delta": {}},
	"reasoning-end":    {"id": {}},
	"tool-input-start": {"id": {}, "name": {}},
	"tool-input-delta": {"id": {}, "delta": {}},
	"tool-input-end":   {"id": {}},
	"tool-call":        {"id": {}, "name": {}, "input": {}, "provider_executed": {}},
	"tool-result":      {"id": {}, "name": {}},
	"source":           {"id": {}, "source_type": {}, "url": {}, "title": {}},
	"usage":            {},
	"finish":           {"finish_reason": {}},
	"error":            {"message": {}},
}

func ValidateEventMapping(mapping EventMapping) error {
	allowed, ok := normalizedEventFields[mapping.Event]
	if !ok {
		return fmt.Errorf("event %q is unsupported", mapping.Event)
	}
	var errs []error
	for name, pointer := range mapping.Fields {
		if pointer == "" || len(pointer) > 1024 || !ValidJSONPointer(pointer) {
			errs = append(errs, fmt.Errorf("fields.%s is not a valid non-root JSON Pointer", name))
		}
		if metadataField, isMetadata := strings.CutPrefix(name, "metadata."); isMetadata {
			if metadataField == "" {
				errs = append(errs, fmt.Errorf("field %q is an unsupported normalized field", name))
			}
			if mapping.MetadataNamespace == "" {
				errs = append(errs, fmt.Errorf("metadata field requires metadata_namespace"))
			}
			continue
		}
		if _, supported := allowed[name]; !supported {
			errs = append(errs, fmt.Errorf("field %q is an unsupported normalized field for event %q", name, mapping.Event))
		}
	}
	return errors.Join(errs...)
}

func ValidateEventMappings(mappings []EventMapping) error {
	var errs []error
	for i, mapping := range mappings {
		if err := ValidateEventMapping(mapping); err != nil {
			errs = append(errs, fmt.Errorf("mappings[%d]: %w", i, err))
		}
		for j := 0; j < i; j++ {
			previous := mappings[j]
			if previous.Source != mapping.Source {
				continue
			}
			if terminalEventMapping(previous.Event) && terminalEventMapping(mapping.Event) {
				errs = append(errs, fmt.Errorf("mappings[%d] source %q declares more than one terminal mapping", i, mapping.Source))
				continue
			}
			if previous.Event == mapping.Event && !eventConditionsMutuallyExclusive(previous.Condition, mapping.Condition) {
				errs = append(errs, fmt.Errorf("mappings[%d] duplicates source %q event %q without mutually exclusive conditions", i, mapping.Source, mapping.Event))
			}
		}
	}
	return errors.Join(errs...)
}

func terminalEventMapping(event string) bool {
	return event == "finish" || event == "error"
}

func eventConditionsMutuallyExclusive(left, right *Predicate) bool {
	if left == nil || right == nil || left.Path == "" || left.Path != right.Path {
		return false
	}
	leftValues, leftFinite := predicateLiteralValues(left)
	rightValues, rightFinite := predicateLiteralValues(right)
	if leftFinite && rightFinite {
		for _, leftValue := range leftValues {
			for _, rightValue := range rightValues {
				if eventLiteralEqual(leftValue, rightValue) {
					return false
				}
			}
		}
		return true
	}
	if excluded, ok := predicateLiteralExclusion(left); ok && rightFinite {
		return literalSetOnlyContains(rightValues, excluded)
	}
	if excluded, ok := predicateLiteralExclusion(right); ok && leftFinite {
		return literalSetOnlyContains(leftValues, excluded)
	}
	return false
}

func predicateLiteralValues(predicate *Predicate) ([]any, bool) {
	switch predicate.Operation {
	case "equals":
		if predicate.Value == nil || predicate.Value.Kind != "literal" {
			return nil, false
		}
		return []any{predicate.Value.Value}, true
	case "matches-enum":
		if len(predicate.Values) == 0 {
			return nil, false
		}
		values := make([]any, 0, len(predicate.Values))
		for _, value := range predicate.Values {
			if value.Kind != "literal" {
				return nil, false
			}
			values = append(values, value.Value)
		}
		return values, true
	default:
		return nil, false
	}
}

func predicateLiteralExclusion(predicate *Predicate) (any, bool) {
	if predicate.Operation != "not-equals" || predicate.Value == nil || predicate.Value.Kind != "literal" {
		return nil, false
	}
	return predicate.Value.Value, true
}

func literalSetOnlyContains(values []any, expected any) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !eventLiteralEqual(value, expected) {
			return false
		}
	}
	return true
}

func eventLiteralEqual(left, right any) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if bytes.Equal(leftData, rightData) {
		return true
	}
	leftNumber, leftOK := numericJSONLiteral(leftData)
	rightNumber, rightOK := numericJSONLiteral(rightData)
	return leftOK && rightOK &&
		leftNumber.negative == rightNumber.negative &&
		leftNumber.digits == rightNumber.digits &&
		leftNumber.exponent.Cmp(rightNumber.exponent) == 0
}

type normalizedJSONNumber struct {
	negative bool
	digits   string
	exponent *big.Int
}

func numericJSONLiteral(data []byte) (*normalizedJSONNumber, bool) {
	text := string(data)
	if text == "" || text != strings.TrimSpace(text) || !json.Valid(data) {
		return nil, false
	}
	unsigned := text
	negative := false
	if unsigned[0] == '-' {
		negative = true
		unsigned = unsigned[1:]
	}
	if unsigned == "" || unsigned[0] < '0' || unsigned[0] > '9' {
		return nil, false
	}
	mantissa := unsigned
	exponentText := ""
	if index := strings.IndexAny(unsigned, "eE"); index >= 0 {
		mantissa = unsigned[:index]
		exponentText = unsigned[index+1:]
	}
	fractionDigits := 0
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		fractionDigits = len(mantissa) - index - 1
		mantissa = mantissa[:index] + mantissa[index+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return &normalizedJSONNumber{digits: "0", exponent: new(big.Int)}, true
	}
	trimmed := strings.TrimRight(digits, "0")
	trailingZeros := len(digits) - len(trimmed)
	exponent := new(big.Int)
	if exponentText != "" {
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return nil, false
		}
	}
	exponent.Add(exponent, big.NewInt(int64(trailingZeros-fractionDigits)))
	return &normalizedJSONNumber{negative: negative, digits: trimmed, exponent: exponent}, true
}

func ValidateUsageMappings(policy *UsagePolicy) error {
	if policy == nil {
		return nil
	}
	var errs []error
	seen := make(map[string]struct{}, len(policy.Mappings))
	hasCacheRead := false
	hasSubtractCacheRead := false
	for i, mapping := range policy.Mappings {
		prefix := fmt.Sprintf("usage.mappings[%d]", i)
		if mapping.Pointer == "" || len(mapping.Pointer) > 1024 || !ValidJSONPointer(mapping.Pointer) {
			errs = append(errs, fmt.Errorf("%s.pointer is not a valid non-root JSON Pointer", prefix))
		}
		if _, duplicate := seen[mapping.Target]; duplicate {
			errs = append(errs, fmt.Errorf("%s duplicates target %q", prefix, mapping.Target))
		}
		seen[mapping.Target] = struct{}{}
		if mapping.Target == "cache_read_tokens" {
			hasCacheRead = true
		}
		switch mapping.Operation {
		case "", "copy", "replace", "accumulate":
		case "subtract-cache-read":
			hasSubtractCacheRead = true
			if mapping.Target != "input_tokens" {
				errs = append(errs, fmt.Errorf("%s subtract-cache-read requires input_tokens target", prefix))
			}
		default:
			errs = append(errs, fmt.Errorf("%s operation %q is unsupported", prefix, mapping.Operation))
		}
	}
	if hasSubtractCacheRead && !hasCacheRead {
		errs = append(errs, fmt.Errorf("usage subtract-cache-read requires a cache_read_tokens mapping"))
	}
	return errors.Join(errs...)
}

func collectIDs(label string, count int, get func(int) string, add func(string, ...any)) map[string]struct{} {
	result := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		id := get(i)
		if !localIDPattern.MatchString(id) {
			add("%s[%d].id is invalid", label, i)
		}
		if _, ok := result[id]; ok {
			add("%s[%d].id %q is duplicated", label, i, id)
		}
		result[id] = struct{}{}
	}
	return result
}

func requireRef(field, value string, known map[string]struct{}, add func(string, ...any)) {
	if _, ok := known[value]; !ok {
		add("%s references unknown id %q", field, value)
	}
}

func validateEndpoint(i int, endpoint Endpoint, credentials map[string]struct{}, add func(string, ...any)) {
	prefix := fmt.Sprintf("endpoints[%d]", i)
	u, err := url.Parse(endpoint.BaseURL)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		add("%s.base_url must be an absolute credential-free URL without query or fragment", prefix)
		return
	}
	for _, host := range endpoint.AllowedHosts {
		if !validEndpointHost(host) {
			add("%s.allowed_hosts contains invalid exact hostname %q", prefix, host)
		}
	}
	if !slices.Contains(endpoint.AllowedSchemes, strings.ToLower(u.Scheme)) {
		add("%s.base_url scheme is not allowed", prefix)
	}
	if !slices.Contains(endpoint.AllowedHosts, strings.ToLower(u.Hostname())) {
		add("%s.base_url host is not allowed", prefix)
	}
	if endpoint.Credential != "" {
		requireRef(prefix+".credential", endpoint.Credential, credentials, add)
	}
}

func validateTemplate(prefix string, value Template, credentials, configProperties map[string]struct{}, add func(string, ...any)) {
	switch value.Kind {
	case "literal":
		if value.Ref != "" || len(value.Parts) != 0 || value.Bytes != 0 {
			add("%s literal may only use value", prefix)
		}
	case "config":
		if value.Ref == "" {
			add("%s requires ref", prefix)
		} else if _, ok := configProperties[value.Ref]; !ok {
			add("%s references unknown configuration property %q", prefix, value.Ref)
		}
		if value.Value != nil || len(value.Parts) != 0 || value.Bytes != 0 {
			add("%s may only use ref", prefix)
		}
	case "context":
		if value.Ref == "" {
			add("%s requires ref", prefix)
		}
		if value.Value != nil || len(value.Parts) != 0 || value.Bytes != 0 {
			add("%s may only use ref", prefix)
		}
	case "credential":
		if value.Ref == "" {
			add("%s requires ref", prefix)
		} else {
			requireRef(prefix+".ref", value.Ref, credentials, add)
		}
		if value.Value != nil || len(value.Parts) != 0 || value.Bytes != 0 {
			add("%s credential may only use ref", prefix)
		}
	case "concat":
		if len(value.Parts) == 0 {
			add("%s concat requires parts", prefix)
		}
		if value.Value != nil || value.Ref != "" || value.Bytes != 0 {
			add("%s concat may only use parts", prefix)
		}
	case "uuid", "unix-time":
		if value.Value != nil || value.Ref != "" || len(value.Parts) != 0 || value.Bytes != 0 {
			add("%s does not accept parameters", prefix)
		}
	case "random-hex":
		if value.Bytes < 1 {
			add("%s random-hex requires bytes", prefix)
		}
		if value.Value != nil || value.Ref != "" || len(value.Parts) != 0 {
			add("%s random-hex may only use bytes", prefix)
		}
	default:
		add("%s has unknown kind %q", prefix, value.Kind)
	}
	for i, part := range value.Parts {
		validateTemplate(fmt.Sprintf("%s.parts[%d]", prefix, i), part, credentials, configProperties, add)
	}
}

func validateHeaderRules(prefix string, rules []HeaderRule, credentials, configProperties map[string]struct{}, add func(string, ...any)) {
	for i, rule := range rules {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if !HeaderNameValid(rule.Name) {
			add("%s.name is not a valid HTTP header name", field)
		}
		if rule.Operation == "delete" {
			if rule.Value != nil {
				add("%s delete must not declare value", field)
			}
			continue
		}
		if rule.Value == nil {
			add("%s requires value", field)
			continue
		}
		validateTemplate(field+".value", *rule.Value, credentials, configProperties, add)
	}
}

func validatePredicate(prefix string, predicate *Predicate, credentials, configProperties map[string]struct{}, add func(string, ...any)) {
	if predicate == nil {
		return
	}
	if predicate.Value != nil {
		validateTemplate(prefix+".value", *predicate.Value, credentials, configProperties, add)
	}
	for i, value := range predicate.Values {
		validateTemplate(fmt.Sprintf("%s.values[%d]", prefix, i), value, credentials, configProperties, add)
	}
}

func validateToolCodec(name string, codec ToolCodec, add func(string, ...any)) {
	if len(codec.Aliases) == 0 && len(codec.PrefixAliases) == 0 {
		add("tool_codecs.%s must declare aliases or prefix_aliases", name)
	}
	host, provider := map[string]struct{}{}, map[string]struct{}{}
	knownTools := map[string]struct{}{}
	for i, alias := range codec.Aliases {
		h := alias.Host
		p := alias.Provider
		knownTools[alias.Host] = struct{}{}
		if codec.CaseFoldInbound {
			h = strings.ToLower(h)
			p = strings.ToLower(p)
		}
		if _, ok := host[h]; ok {
			add("tool_codecs.%s.aliases[%d] duplicates host name", name, i)
		}
		if _, ok := provider[p]; ok {
			add("tool_codecs.%s.aliases[%d] is not bidirectionally unique", name, i)
		}
		host[h], provider[p] = struct{}{}, struct{}{}
	}
	prefixHosts := make([]string, 0, len(codec.PrefixAliases))
	prefixProviders := make([]string, 0, len(codec.PrefixAliases))
	hasDeferred := false
	for i, alias := range codec.PrefixAliases {
		h := alias.HostPrefix
		p := alias.ProviderPrefix
		if !toolPrefixPattern.MatchString(h) || !toolPrefixPattern.MatchString(p) {
			add("tool_codecs.%s.prefix_aliases[%d] prefixes must use tool-name characters", name, i)
			continue
		}
		if codec.CaseFoldInbound {
			h = strings.ToLower(h)
			p = strings.ToLower(p)
		}
		for _, existing := range prefixHosts {
			if strings.HasPrefix(h, existing) || strings.HasPrefix(existing, h) {
				add("tool_codecs.%s.prefix_aliases[%d] overlaps another host prefix", name, i)
			}
		}
		for _, existing := range prefixProviders {
			if strings.HasPrefix(p, existing) || strings.HasPrefix(existing, p) {
				add("tool_codecs.%s.prefix_aliases[%d] is not bidirectionally unique", name, i)
			}
		}
		for exact := range host {
			if strings.HasPrefix(exact, h) {
				add("tool_codecs.%s.prefix_aliases[%d] overlaps an exact host alias", name, i)
			}
		}
		for exact := range provider {
			if strings.HasPrefix(exact, p) {
				add("tool_codecs.%s.prefix_aliases[%d] overlaps an exact provider alias", name, i)
			}
		}
		prefixHosts = append(prefixHosts, h)
		prefixProviders = append(prefixProviders, p)
		hasDeferred = hasDeferred || alias.DeferLoading
	}
	if codec.ToolSearch != "" && codec.ToolSearch != "regex" && codec.ToolSearch != "bm25" {
		add("tool_codecs.%s.tool_search has unknown mode %q", name, codec.ToolSearch)
	}
	if hasDeferred && codec.ToolSearch == "" {
		add("tool_codecs.%s.tool_search is required when prefix aliases defer loading", name)
	}
	if codec.ToolSearch != "" && !hasDeferred {
		add("tool_codecs.%s.tool_search requires a deferred prefix alias", name)
	}
	parameterHosts := map[string]map[string]struct{}{}
	parameterProviders := map[string]map[string]struct{}{}
	for i, mapping := range codec.Parameters {
		_, known := knownTools[mapping.Tool]
		if !known {
			for _, alias := range codec.PrefixAliases {
				if strings.HasPrefix(mapping.Tool, alias.HostPrefix) {
					known = true
					break
				}
			}
		}
		if !known {
			add("tool_codecs.%s.parameters[%d] references unknown host tool %q", name, i, mapping.Tool)
		}
		if strings.TrimSpace(mapping.Host) == "" || strings.TrimSpace(mapping.Provider) == "" {
			add("tool_codecs.%s.parameters[%d] names must be non-empty", name, i)
		}
		if parameterHosts[mapping.Tool] == nil {
			parameterHosts[mapping.Tool] = map[string]struct{}{}
			parameterProviders[mapping.Tool] = map[string]struct{}{}
		}
		h, p := mapping.Host, mapping.Provider
		if codec.CaseFoldInbound {
			h, p = strings.ToLower(h), strings.ToLower(p)
		}
		if _, ok := parameterHosts[mapping.Tool][h]; ok {
			add("tool_codecs.%s.parameters[%d] duplicates host parameter", name, i)
		}
		if _, ok := parameterProviders[mapping.Tool][p]; ok {
			add("tool_codecs.%s.parameters[%d] is not bidirectionally unique", name, i)
		}
		parameterHosts[mapping.Tool][h] = struct{}{}
		parameterProviders[mapping.Tool][p] = struct{}{}
	}
}

func validateOperation(i int, operation Operation, endpoints, operations, clientIdentities, usageOperations map[string]struct{}, c Capabilities, configProperties map[string]struct{}, add func(string, ...any)) {
	prefix := fmt.Sprintf("operations[%d]", i)
	requireRef(prefix+".endpoint", operation.Endpoint, endpoints, add)
	if operation.ClientIdentity != "" {
		requireRef(prefix+".client_identity", operation.ClientIdentity, clientIdentities, add)
		if _, ok := usageOperations[operation.ID]; !ok {
			add("%s.client_identity is only supported for operation-sourced usage", prefix)
		}
	}
	validateHeaderRules(prefix+".headers", operation.Headers, collectCredentialRefs(c), configProperties, add)
	if operation.RequestTransform != "" {
		if _, ok := c.JSONTransforms[operation.RequestTransform]; !ok {
			add("%s.request_transform is unknown", prefix)
		}
	}
	if operation.ResponseTransform != "" {
		if _, ok := c.JSONTransforms[operation.ResponseTransform]; !ok {
			add("%s.response_transform is unknown", prefix)
		}
	}
	if operation.PromptTransform != "" {
		if _, ok := c.PromptTransforms[operation.PromptTransform]; !ok {
			add("%s.prompt_transform is unknown", prefix)
		}
	}
	if operation.RoleMap != "" {
		if _, ok := c.RoleMaps[operation.RoleMap]; !ok {
			add("%s.role_map is unknown", prefix)
		}
	}
	if operation.ToolCodec != "" {
		if _, ok := c.ToolCodecs[operation.ToolCodec]; !ok {
			add("%s.tool_codec is unknown", prefix)
		}
	}
	if operation.Streaming != nil {
		if operation.Transport == "websocket-json" && operation.Streaming.EventSource != "websocket-json" {
			add("%s WebSocket transport requires websocket-json event source", prefix)
		}
		if operation.Transport == "sse" && operation.Streaming.EventSource != "sse-data-json" {
			add("%s SSE transport requires sse-data-json event source", prefix)
		}
		if operation.Streaming.MaxEventBytes == 0 {
			add("%s.streaming.max_event_bytes must be explicit", prefix)
		}
		if len(operation.Streaming.Mappings) > 0 && operation.Streaming.EventTypePointer == "" {
			add("%s.streaming.event_type_pointer is required with event mappings", prefix)
		}
		if operation.Streaming.EventTypePointer != "" && (len(operation.Streaming.EventTypePointer) > 1024 || !ValidJSONPointer(operation.Streaming.EventTypePointer)) {
			add("%s.streaming.event_type_pointer is not a valid non-root JSON Pointer", prefix)
		}
		credentials := collectCredentialRefs(c)
		hasFinish := false
		if err := ValidateEventMappings(operation.Streaming.Mappings); err != nil {
			add("%s.streaming: %v", prefix, err)
		}
		for j, mapping := range operation.Streaming.Mappings {
			if mapping.Event == "finish" {
				hasFinish = true
			}
			validatePredicate(fmt.Sprintf("%s.streaming.mappings[%d].condition", prefix, j), mapping.Condition, credentials, configProperties, add)
		}
		if operation.Kind == "inference" && !hasFinish {
			add("%s.streaming requires a mapped finish event", prefix)
		}
	}
	if operation.Retry != nil && operation.Retry.MaxAttempts < 1 {
		add("%s.retry.max_attempts must be positive", prefix)
	}
	if operation.Continuation != nil {
		mode := operation.Continuation.Mode
		if mode == "previous-response" && operation.Protocol != "openai-responses" {
			add("%s previous-response requires openai-responses", prefix)
		}
		if mode == "previous-interaction" && operation.Protocol != "gemini-interactions" {
			add("%s previous-interaction requires gemini-interactions", prefix)
		}
	}
	if operation.Compaction != nil && operation.Compaction.Operation != "" {
		requireRef(prefix+".compaction.operation", operation.Compaction.Operation, operations, add)
	}
}

func collectCredentialRefs(c Capabilities) map[string]struct{} {
	result := make(map[string]struct{}, len(c.Credentials))
	for _, credential := range c.Credentials {
		result[credential.ID] = struct{}{}
	}
	return result
}

func validEndpointHost(value string) bool {
	if value == "" || value != strings.ToLower(value) || strings.HasSuffix(value, ".") || strings.ContainsAny(value, "/*@[]") {
		return false
	}
	return net.ParseIP(value) != nil || (len(value) <= 253 && hostnamePattern.MatchString(value))
}

func safeBundlePath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") {
		return false
	}
	clean := path.Clean(value)
	return clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

// HeaderNameValid is exposed for the future semantic loader and tests.
func HeaderNameValid(name string) bool {
	return httpguts.ValidHeaderFieldName(name)
}
