package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	pluginIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	localIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	featureIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	hostnamePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
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
		if len(model.Modalities.Input) == 0 || len(model.Modalities.Output) == 0 {
			add("%s.modalities input and output are required", prefix)
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
	validateConfiguration(m.Configuration, add)
	validateCapabilities(m, add)
	return errors.Join(errs...)
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
	inferenceOperations := 0
	for i, operation := range c.Operations {
		validateOperation(i, operation, endpoints, operations, c, configProperties, add)
		if operation.Kind == "inference" {
			inferenceOperations++
		}
	}
	if inferenceOperations != 1 {
		add("capabilities.operations must declare exactly one inference operation")
	}
	validateAnthropicPolicy(c.Anthropic, c.Operations, add)
	if c.Usage != nil && c.Usage.Operation != "" {
		requireRef("usage.operation", c.Usage.Operation, operations, add)
	}
	if c.Instructions != nil {
		if _, ok := c.Instructions.Profiles[c.Instructions.Default]; !ok {
			add("instructions.default references unknown profile")
		}
		for id, file := range c.Instructions.Profiles {
			if !safeBundlePath(file) {
				add("instructions.profiles.%s is not a safe bundle-relative path", id)
			}
		}
	}
	validateImagePolicy(c.Images, add)
	validateRuntimeControls(c.RuntimeControls, add)
	validateErrorMappings(c.Errors, add)
	metadata := map[string]struct{}{}
	for i, contract := range c.Metadata {
		key := fmt.Sprintf("%s/%d/%s", contract.Namespace, contract.Version, contract.Scope)
		if _, ok := metadata[key]; ok {
			add("metadata[%d] duplicates %s", i, key)
		}
		metadata[key] = struct{}{}
		if contract.Schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			add("metadata[%d].schema must declare Draft 2020-12", i)
		}
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

func validateAnthropicPolicy(policy *AnthropicPolicy, operations []Operation, add func(string, ...any)) {
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
	if identity := policy.ClientIdentity; identity != nil {
		if identity.Environment != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(identity.Environment) {
			add("anthropic.client_identity.environment is invalid")
		}
		if identity.LatestURL != "" {
			u, err := url.Parse(identity.LatestURL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				add("anthropic.client_identity.latest_url must be an absolute credential-free HTTPS URL without fragment")
			}
		}
		pattern, err := regexp.Compile(identity.VersionPattern)
		if err != nil {
			add("anthropic.client_identity.version_pattern is invalid")
		} else if !pattern.MatchString(identity.FallbackVersion) {
			add("anthropic.client_identity.fallback_version does not match version_pattern")
		}
		if strings.Count(identity.UserAgentFormat, "{version}") != 1 {
			add("anthropic.client_identity.user_agent_format must contain {version} exactly once")
		}
	}
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
		if strings.Count(billing.Format, "{version}") != 1 || strings.Count(billing.Format, "{fingerprint}") != 1 {
			add("anthropic.billing.format must contain {version} and {fingerprint} exactly once")
		}
	}
}

func validateImagePolicy(policy *ImagePolicy, add func(string, ...any)) {
	if policy == nil {
		return
	}
	for _, mediaType := range policy.AcceptedMediaTypes {
		if !slices.Contains([]string{"image/gif", "image/jpeg", "image/png", "image/webp"}, mediaType) {
			add("images.accepted_media_types contains unsupported media type %q", mediaType)
		}
	}
	if policy.OutputMediaType != "" && policy.OutputMediaType != "image/jpeg" && policy.OutputMediaType != "image/png" {
		add("images.output_media_type %q is not supported by the core image encoder", policy.OutputMediaType)
	}
	for _, quality := range policy.QualitySteps {
		if quality < 1 || quality > 100 {
			add("images.quality_steps contains invalid JPEG quality %d", quality)
		}
	}
}

func validateRuntimeControls(controls []RuntimeControl, add func(string, ...any)) {
	ids := map[string]struct{}{}
	for i, control := range controls {
		prefix := fmt.Sprintf("runtime_controls[%d]", i)
		if _, ok := ids[control.ID]; ok {
			add("%s.id %q is duplicated", prefix, control.ID)
		}
		ids[control.ID] = struct{}{}
		if control.RequestPath != "" && !validJSONPointer(control.RequestPath) {
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

func validateErrorMappings(mappings []ErrorMapping, add func(string, ...any)) {
	for i, mapping := range mappings {
		prefix := fmt.Sprintf("errors[%d]", i)
		for _, status := range mapping.Statuses {
			if status < 100 || status > 599 {
				add("%s.statuses contains invalid HTTP status %d", prefix, status)
			}
		}
		if mapping.CodePointer != "" && !validJSONPointer(mapping.CodePointer) {
			add("%s.code_pointer is not a valid JSON Pointer", prefix)
		}
		if mapping.MessagePointer != "" && !validJSONPointer(mapping.MessagePointer) {
			add("%s.message_pointer is not a valid JSON Pointer", prefix)
		}
	}
}

func validJSONPointer(value string) bool {
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
	parameterHosts := map[string]map[string]struct{}{}
	parameterProviders := map[string]map[string]struct{}{}
	for i, mapping := range codec.Parameters {
		if _, ok := knownTools[mapping.Tool]; !ok {
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

func validateOperation(i int, operation Operation, endpoints, operations map[string]struct{}, c Capabilities, configProperties map[string]struct{}, add func(string, ...any)) {
	prefix := fmt.Sprintf("operations[%d]", i)
	requireRef(prefix+".endpoint", operation.Endpoint, endpoints, add)
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
		if operation.Streaming.MaxEventBytes == 0 {
			add("%s.streaming.max_event_bytes must be explicit", prefix)
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
