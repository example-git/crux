package providerregistry

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

func ValidateActivation(registration Registration) error {
	switch registration.QuotaCredential {
	case "":
	case QuotaCredentialAccessToken:
		if registration.Quota == nil {
			return activationError(registration, fmt.Errorf("quota credential requires a quota executor"))
		}
	case QuotaCredentialRefreshToken:
		if registration.ProviderID != "copilot" || registration.Construction != ConstructionCopilot || registration.Manifest != nil || registration.Quota == nil || registration.OAuth == nil {
			return activationError(registration, fmt.Errorf("refresh-token quota credentials are restricted to core Copilot"))
		}
	default:
		return activationError(registration, fmt.Errorf("quota credential %q is unsupported", registration.QuotaCredential))
	}
	if registration.Manifest == nil {
		return nil
	}
	if err := validateManifestErrorBindings(registration); err != nil {
		return activationError(registration, err)
	}
	if _, err := manifest.CompileMetadataContracts(registration.Metadata); err != nil {
		return activationError(registration, err)
	}
	if err := validateManifestOperationBindings(registration); err != nil {
		return activationError(registration, err)
	}
	if !reflect.DeepEqual(registration.Images, registration.Manifest.Capabilities.Images) {
		return activationError(registration, fmt.Errorf("registered image policy does not match manifest declaration"))
	}
	if err := manifest.ValidateImagePolicyExecution(registration.Images); err != nil {
		return activationError(registration, err)
	}
	if registration.Images != nil && registration.Images.HistoryBudget != nil {
		expected := integratedCodexImagePolicy()
		if registration.Construction != ConstructionCodex || registration.CompatibilityAdapter != ConstructionCodex || !constructionDelegated(registration) || !reflect.DeepEqual(registration.Images, expected) {
			return activationError(registration, fmt.Errorf("image history policy is unavailable for construction %q", registration.Construction))
		}
	}
	if constructionDelegated(registration) {
		if err := validateDelegatedConstruction(registration); err != nil {
			return activationError(registration, err)
		}
		return nil
	}
	operation := registration.Operation
	if operation == nil {
		return nil
	}
	if err := validateNativeTransport(registration.Construction, operation); err != nil {
		return activationError(registration, err)
	}
	if err := validateNativeLifecycle(registration.Construction, operation); err != nil {
		return activationError(registration, err)
	}
	for _, control := range registration.RuntimeControls {
		if control.RequestPath == "" {
			return activationError(registration, fmt.Errorf("runtime control %q has no request path", control.ID))
		}
	}
	switch registration.Construction {
	case ConstructionOpenAIResponses:
		for _, control := range registration.RuntimeControls {
			if control.Scope == "request" {
				return activationError(registration, fmt.Errorf("runtime control %q cannot be supplied to native OpenAI Responses construction", control.ID))
			}
		}
		if err := validateOpenAIResponsesOperation(operation); err != nil {
			return activationError(registration, err)
		}
		if err := validateNativeUsage(registration); err != nil {
			return activationError(registration, err)
		}
		if err := validateOpenAIResponsesMetadata(operation, registration.Metadata); err != nil {
			return activationError(registration, err)
		}
		if err := validateOpenAIResponsesToolCodec(operation.ToolCodec); err != nil {
			return activationError(registration, err)
		}
	case ConstructionGeminiContent, ConstructionGeminiInteraction, ConstructionGenericJSON:
		if err := validateInferenceTimeouts(operation); err != nil {
			return activationError(registration, err)
		}
		if err := validateDeclarativeFraming(operation); err != nil {
			return activationError(registration, err)
		}
		if err := validateDeclarativeRuntimeControls(registration.RuntimeControls); err != nil {
			return activationError(registration, err)
		}
		if operation.ToolCodec != nil {
			return activationError(registration, fmt.Errorf("tool codec is unavailable for declarative construction %q", registration.Construction))
		}
		if err := validateDeclarativeUsage(registration); err != nil {
			return activationError(registration, err)
		}
		if err := validateDeclarativeMetadata(operation, registration.Metadata); err != nil {
			return activationError(registration, err)
		}
	case ConstructionAnthropicMessages:
		if err := validateInferenceTimeouts(operation); err != nil {
			return activationError(registration, err)
		}
		if err := validateNativeUsage(registration); err != nil {
			return activationError(registration, err)
		}
		if len(registration.Metadata) > 0 {
			return activationError(registration, fmt.Errorf("metadata contracts are unavailable for native Anthropic Messages construction"))
		}
		if len(registration.RuntimeControls) > 0 {
			return activationError(registration, fmt.Errorf("runtime controls are unavailable for native Anthropic Messages construction"))
		}
		if operation.Anthropic == nil {
			if operation.Retry.MaxAttempts > 1 || operation.RequestTransform != nil || operation.ResponseTransform != nil || operation.PromptTransform != nil || operation.RoleMap != nil || operation.ToolCodec != nil {
				return activationError(registration, fmt.Errorf("manifest operation policy requires the manifest-aware Anthropic adapter"))
			}
			break
		}
		if err := validateAnthropicTransforms(operation); err != nil {
			return activationError(registration, err)
		}
		if operation.RequestTransform != nil {
			for _, item := range operation.RequestTransform.Operations {
				if item.Operation != "delete" {
					return activationError(registration, fmt.Errorf("Anthropic request transform operation %q is unavailable", item.Operation))
				}
			}
		}
	}
	return nil
}

func validateManifestErrorBindings(registration Registration) error {
	declared := registration.Manifest.Capabilities.Errors
	if err := manifest.ValidateErrorMappings(declared); err != nil {
		return err
	}
	if !errorMappingsEqual(registration.Errors, declared) {
		return fmt.Errorf("registered error mappings do not match the manifest")
	}
	if registration.Operation != nil && !errorMappingsEqual(registration.Operation.Errors, declared) {
		return fmt.Errorf("inference operation error mappings do not match the manifest")
	}
	operationIDs := make([]string, 0, len(registration.Operations))
	for id := range registration.Operations {
		operationIDs = append(operationIDs, id)
	}
	slices.Sort(operationIDs)
	for _, id := range operationIDs {
		operation := registration.Operations[id]
		if operation == nil {
			continue
		}
		var expected []manifest.ErrorMapping
		if operation.Kind == "inference" || operation.Kind == "compaction" || registration.Operation != nil && operation.ID == registration.Operation.ID {
			expected = declared
		}
		if !errorMappingsEqual(operation.Errors, expected) {
			return fmt.Errorf("compiled operation error mappings do not match manifest scope for %q", id)
		}
	}
	retryable := false
	for _, mapping := range declared {
		if mapping.Retryable {
			retryable = true
			if registration.Construction == ConstructionAnthropicMessages && len(mapping.Statuses) == 0 {
				return fmt.Errorf("retryable Anthropic error mapping requires an HTTP status predicate")
			}
		}
	}
	if !retryable {
		return nil
	}
	if registration.Operation == nil {
		return fmt.Errorf("retryable error mapping has no inference operation")
	}
	operations := []*providertransport.Operation{registration.Operation}
	seen := map[string]struct{}{registration.Operation.ID: {}}
	for _, id := range operationIDs {
		operation := registration.Operations[id]
		if operation == nil || operation.Kind != "compaction" {
			continue
		}
		if _, ok := seen[operation.ID]; ok {
			continue
		}
		seen[operation.ID] = struct{}{}
		operations = append(operations, operation)
	}
	for _, operation := range operations {
		if operation.Retry.MaxAttempts <= 1 {
			return fmt.Errorf("retryable error mapping has no retry attempt budget for operation %q", operation.ID)
		}
		if operation.Retry.ReplayRequirement == "never" {
			return fmt.Errorf("retryable error mapping forbids request replay for operation %q", operation.ID)
		}
	}
	return nil
}

func errorMappingsEqual(left, right []manifest.ErrorMapping) bool {
	return len(left) == 0 && len(right) == 0 || reflect.DeepEqual(left, right)
}

func validateManifestOperationBindings(registration Registration) error {
	if registration.Manifest == nil {
		return nil
	}
	declarations := registration.Manifest.Capabilities.Operations
	referenced := make(map[string]struct{}, len(declarations))
	compatibility := registration.Manifest.Capabilities.Compatibility
	delegated := func(capability string) bool {
		return compatibility != nil && slices.Contains(compatibility.Delegates, capability)
	}
	if operation := registration.Operation; operation != nil {
		referenced[operation.ID] = struct{}{}
		if operation.Compaction != nil && operation.Compaction.Mode == "remote-operation" {
			referenced[operation.Compaction.Operation] = struct{}{}
		}
	}
	if registration.Identity != nil && !delegated("identity") {
		for _, operation := range declarations {
			if operation.Kind == "account" {
				referenced[operation.ID] = struct{}{}
				break
			}
		}
	}
	if policy := registration.Usage; policy != nil && policy.Source == "operation" {
		referenced[policy.Operation] = struct{}{}
		if !delegated("usage") {
			for _, setup := range policy.Setup {
				referenced[setup.Operation] = struct{}{}
			}
		}
	}
	for index, operation := range declarations {
		if _, ok := referenced[operation.ID]; !ok {
			return fmt.Errorf("operation %q at index %d has no host executor", operation.ID, index)
		}
	}
	return nil
}

func validateDeclarativeRuntimeControls(controls []manifest.RuntimeControl) error {
	for _, control := range controls {
		if control.Scope != "model" {
			return fmt.Errorf("runtime control %q scope %q is unavailable for declarative construction", control.ID, control.Scope)
		}
	}
	return nil
}

func validateDeclarativeUsage(registration Registration) error {
	policy := registration.Usage
	if registration.Operation.Streaming != nil {
		for _, mapping := range registration.Operation.Streaming.Mappings {
			if mapping.Event == "usage" && (policy == nil || policy.Source != "stream") {
				return fmt.Errorf("usage event requires stream-sourced usage policy")
			}
		}
	}
	if policy == nil {
		return nil
	}
	if policy.Source == "operation" && len(policy.Mappings) > 0 {
		return fmt.Errorf("operation-sourced usage mappings are unavailable")
	}
	if err := manifest.ValidateUsageMappings(policy); err != nil {
		return err
	}
	switch policy.Source {
	case "response":
		if registration.Operation.Key.Transport != "http-json" {
			return fmt.Errorf("response-sourced usage requires http-json transport")
		}
	case "stream":
		if registration.Operation.Key.Transport != "sse" {
			return fmt.Errorf("stream-sourced usage requires SSE transport")
		}
	case "operation":
		if registration.Quota == nil {
			return fmt.Errorf("operation-sourced usage has no compiled quota executor")
		}
		return nil
	default:
		return fmt.Errorf("usage source %q is unavailable for declarative construction %q", policy.Source, registration.Construction)
	}
	if policy.Operation != "" || len(policy.Setup) > 0 || len(policy.Windows) > 0 || len(policy.PlanPointers) > 0 {
		return fmt.Errorf("usage source %q declares operation-only fields", policy.Source)
	}
	if policy.Source == "stream" {
		for _, mapping := range policy.Mappings {
			if mapping.Operation == "accumulate" {
				return fmt.Errorf("stream-sourced usage accumulation is unavailable")
			}
		}
	}
	if policy.Fallback != "zero" {
		return fmt.Errorf("usage fallback %q is unavailable for declarative construction %q", policy.Fallback, registration.Construction)
	}
	return nil
}

func validateInferenceTimeouts(operation *providertransport.Operation) error {
	if operation.Timeouts == nil || operation.Timeouts.IdleSeconds <= 0 {
		return nil
	}
	if operation.Key.Transport != "sse" {
		return fmt.Errorf("operation %q idle timeout requires SSE inference transport", operation.ID)
	}
	return nil
}

func validateAnthropicTransforms(operation *providertransport.Operation) error {
	if operation.ResponseTransform != nil {
		return fmt.Errorf("response transforms are unavailable for the manifest-aware Anthropic adapter")
	}
	if operation.PromptTransform != nil {
		prefixes := operation.Anthropic.SystemLinePrefixes
		for _, item := range operation.PromptTransform.Operations {
			if item.Operation != "remove-lines-with-prefix" || item.Role != "system" || item.Text != nil || item.When != nil || !slices.Contains(prefixes, item.Prefix) {
				return fmt.Errorf("prompt transform is not executed by the manifest-aware Anthropic adapter")
			}
		}
	}
	if mapping := operation.RoleMap; mapping != nil {
		if mapping.System != "system" || mapping.Developer != "system" || mapping.User != "user" || mapping.Assistant != "assistant" || mapping.Tool != "tool" || mapping.Unknown != "reject" {
			return fmt.Errorf("role map is not executed by the manifest-aware Anthropic adapter")
		}
	}
	return nil
}

func validateNativeUsage(registration Registration) error {
	if registration.Usage == nil {
		return nil
	}
	if registration.Usage.Source == "operation" {
		if registration.Quota == nil {
			return fmt.Errorf("operation-sourced usage has no compiled quota executor")
		}
		return nil
	}
	if registration.Construction == ConstructionOpenAIResponses && nativeOpenAIResponsesUsage(registration.Usage) {
		return nil
	}
	return fmt.Errorf("usage source %q is unavailable for native %s construction", registration.Usage.Source, registration.Construction)
}

func nativeOpenAIResponsesUsage(policy *manifest.UsagePolicy) bool {
	if policy.Source != "stream" || policy.Operation != "" || policy.Fallback != "estimate" || len(policy.Windows) != 0 || len(policy.Mappings) != 3 {
		return false
	}
	expected := map[string]manifest.UsageMapping{
		"input_tokens":      {Target: "input_tokens", Pointer: "/response/usage/input_tokens", Operation: "subtract-cache-read"},
		"output_tokens":     {Target: "output_tokens", Pointer: "/response/usage/output_tokens", Operation: "replace"},
		"cache_read_tokens": {Target: "cache_read_tokens", Pointer: "/response/usage/input_tokens_details/cached_tokens", Operation: "replace"},
	}
	seen := make(map[string]struct{}, len(policy.Mappings))
	for _, mapping := range policy.Mappings {
		candidate, ok := expected[mapping.Target]
		if !ok || mapping != candidate {
			return false
		}
		if _, duplicate := seen[mapping.Target]; duplicate {
			return false
		}
		seen[mapping.Target] = struct{}{}
	}
	return len(seen) == len(expected)
}

func validateOpenAIResponsesOperation(operation *providertransport.Operation) error {
	if operation.Streaming != nil {
		return fmt.Errorf("operation %q streaming policy is unavailable for native OpenAI Responses construction; the fixed native parser owns SSE semantics", operation.ID)
	}
	if operation.PromptTransform != nil {
		for _, item := range operation.PromptTransform.Operations {
			if item.Operation == "join-adjacent-role" {
				return fmt.Errorf("operation %q prompt transform %q is unavailable for native OpenAI Responses construction because opaque input items separate role messages", operation.ID, item.Operation)
			}
		}
	}
	if operation.Timeouts == nil {
		return nil
	}
	if operation.Timeouts.ConnectSeconds > 0 {
		return fmt.Errorf("operation %q connect timeout hint is unavailable for native OpenAI Responses construction", operation.ID)
	}
	if operation.Timeouts.RequestSeconds > 0 {
		return fmt.Errorf("operation %q request timeout hint is unavailable for native OpenAI Responses construction", operation.ID)
	}
	if operation.Timeouts.IdleSeconds > 0 {
		return fmt.Errorf("operation %q idle timeout hint is unavailable for native OpenAI Responses construction", operation.ID)
	}
	return nil
}

func validateOpenAIResponsesToolCodec(codec *manifest.ToolCodec) error {
	if codec == nil {
		return nil
	}
	if len(codec.PrefixAliases) > 0 {
		return fmt.Errorf("tool codec prefix aliases are unavailable for native OpenAI Responses construction")
	}
	if codec.ToolSearch != "" {
		return fmt.Errorf("tool codec search mode %q is unavailable for native OpenAI Responses construction", codec.ToolSearch)
	}
	supported := map[string]struct{}{
		"definitions":       {},
		"prompt-references": {},
		"history-calls":     {},
		"stream-events":     {},
	}
	for _, surface := range codec.Surfaces {
		if _, ok := supported[surface]; !ok {
			return fmt.Errorf("tool codec surface %q is unavailable for native OpenAI Responses construction", surface)
		}
	}
	return nil
}

func validateDeclarativeMetadata(operation *providertransport.Operation, contracts []manifest.MetadataContract) error {
	if len(contracts) == 0 {
		return nil
	}
	if operation.Streaming == nil {
		return fmt.Errorf("metadata contracts require declarative event mappings")
	}
	declared := make(map[string]manifest.MetadataContract, len(contracts))
	used := make(map[string]bool, len(contracts))
	for _, contract := range contracts {
		if _, exists := declared[contract.Namespace]; exists {
			return fmt.Errorf("metadata namespace %q is declared more than once", contract.Namespace)
		}
		if contract.Version != 1 {
			return fmt.Errorf("metadata namespace %q declares unsupported envelope version %d", contract.Namespace, contract.Version)
		}
		if contract.RequiredForReplay {
			return fmt.Errorf("metadata namespace %q replay requirement is unavailable for declarative construction", contract.Namespace)
		}
		if contract.LegacyProjection != "" {
			return fmt.Errorf("metadata namespace %q legacy projection %q is unavailable for declarative construction", contract.Namespace, contract.LegacyProjection)
		}
		declared[contract.Namespace] = contract
	}
	for _, mapping := range operation.Streaming.Mappings {
		if mapping.MetadataNamespace == "" {
			continue
		}
		emitsMetadata := false
		for field := range mapping.Fields {
			if strings.HasPrefix(field, "metadata.") {
				emitsMetadata = true
				break
			}
		}
		if !emitsMetadata {
			return fmt.Errorf("event %q metadata namespace %q has no metadata fields", mapping.Source, mapping.MetadataNamespace)
		}
		contract, ok := declared[mapping.MetadataNamespace]
		if !ok {
			return fmt.Errorf("event %q references undeclared metadata namespace %q", mapping.Source, mapping.MetadataNamespace)
		}
		scope := metadataEventScope(mapping.Event)
		if scope == "" || contract.Scope != scope {
			return fmt.Errorf("metadata namespace %q scope %q cannot be preserved from event %q", contract.Namespace, contract.Scope, mapping.Event)
		}
		used[contract.Namespace] = true
	}
	for _, contract := range contracts {
		if !used[contract.Namespace] {
			return fmt.Errorf("metadata namespace %q is not emitted by an inference event mapping", contract.Namespace)
		}
	}
	return nil
}

func metadataEventScope(event string) string {
	switch event {
	case "text-end":
		return "text"
	case "reasoning-end":
		return "reasoning"
	case "tool-call", "tool-input-end":
		return "tool-call"
	case "tool-result":
		return "tool-result"
	case "finish", "usage":
		return "message"
	default:
		return ""
	}
}

func validateDelegatedConstruction(registration Registration) error {
	operation := registration.Operation
	if operation == nil {
		return fmt.Errorf("delegated construction has no inference operation")
	}
	compatibility := registration.Manifest.Capabilities.Compatibility
	if compatibility == nil || registration.CompatibilityAdapter == "" || registration.Construction != registration.CompatibilityAdapter {
		return fmt.Errorf("delegated construction does not match its compatibility adapter")
	}
	var protocol, transport string
	switch registration.Construction {
	case ConstructionCodex:
		protocol = string(ConstructionOpenAIResponses)
		transport = "websocket-json"
	case ConstructionGeminiAntigravity:
		protocol = string(ConstructionGeminiContent)
		transport = "sse"
	default:
		return fmt.Errorf("compatibility construction %q is unsupported", registration.Construction)
	}
	if err := operation.ValidateSelection(protocol, transport); err != nil {
		return err
	}
	if operation.Method != http.MethodPost {
		return fmt.Errorf("operation %q method %q is unavailable for compatibility construction %q", operation.ID, operation.Method, registration.Construction)
	}
	if operation.ClientIdentity != nil {
		return fmt.Errorf("operation %q client identity is unavailable for compatibility construction %q", operation.ID, registration.Construction)
	}
	if operation.RequestTransform != nil {
		return fmt.Errorf("operation %q request transform is unavailable for compatibility construction %q", operation.ID, registration.Construction)
	}
	if operation.ResponseTransform != nil {
		if registration.Construction != ConstructionGeminiAntigravity || !geminiResponseTransformEquivalent(operation.ResponseTransform) {
			return fmt.Errorf("operation %q response transform is unavailable for compatibility construction %q", operation.ID, registration.Construction)
		}
	}
	if operation.PromptTransform != nil {
		return fmt.Errorf("operation %q prompt transform is unavailable for compatibility construction %q", operation.ID, registration.Construction)
	}
	if operation.ToolCodec != nil {
		return fmt.Errorf("operation %q tool codec is unavailable for compatibility construction %q", operation.ID, registration.Construction)
	}
	if operation.RoleMap != nil {
		mismatches := geminiRoleMapMismatches(operation.RoleMap)
		if registration.Construction != ConstructionGeminiAntigravity || len(mismatches) > 0 {
			return fmt.Errorf("operation %q role map is unavailable for compatibility construction %q; mismatched semantics: %s", operation.ID, registration.Construction, strings.Join(mismatches, ","))
		}
	}
	if operation.Anthropic != nil || operation.SystemInstruction != nil {
		return fmt.Errorf("operation %q Anthropic policy is unavailable for compatibility construction %q", operation.ID, registration.Construction)
	}
	if registration.Construction == ConstructionCodex {
		if operation.Path != "/" {
			return fmt.Errorf("operation %q path %q is unavailable for Codex compatibility construction", operation.ID, operation.Path)
		}
	}
	delegated := func(capability string) bool {
		return slices.Contains(compatibility.Delegates, capability)
	}
	for _, capability := range compatibility.Delegates {
		switch capability {
		case "construction":
		case "oauth":
			if registration.OAuth == nil {
				return fmt.Errorf("delegated OAuth has no compatibility executor")
			}
		case "identity":
			if registration.Identity == nil {
				return fmt.Errorf("delegated identity has no compatibility executor")
			}
		case "usage":
			if registration.Quota == nil {
				return fmt.Errorf("delegated usage has no compatibility executor")
			}
		case "runtime":
			if registration.Runtime == nil {
				return fmt.Errorf("delegated runtime controls have no compatibility executor")
			}
		case "reasoning":
			if registration.Reasoning == nil {
				return fmt.Errorf("delegated reasoning has no compatibility executor")
			}
		default:
			return fmt.Errorf("delegated capability %q is unsupported", capability)
		}
	}
	if expected := delegatedStreamingPolicy(registration.Construction); operation.Streaming != nil && !reflect.DeepEqual(operation.Streaming, expected) {
		return fmt.Errorf("operation %q streaming policy does not match compatibility construction %q", operation.ID, registration.Construction)
	}
	if expected := delegatedContinuationPolicy(registration.Construction); operation.Continuation != nil && !reflect.DeepEqual(operation.Continuation, expected) {
		return fmt.Errorf("operation %q continuation policy does not match compatibility construction %q", operation.ID, registration.Construction)
	}
	if expected := delegatedCompactionPolicy(registration.Construction); operation.Compaction != nil && !reflect.DeepEqual(operation.Compaction, expected) {
		return fmt.Errorf("operation %q compaction policy does not match compatibility construction %q", operation.ID, registration.Construction)
	}
	if err := validateDelegatedCompactionOperation(registration, operation); err != nil {
		return err
	}
	if declaration := declaredInferenceOperation(registration); declaration != nil && declaration.Retry != nil {
		if expected := delegatedRetryPolicy(registration.Construction); !retryPoliciesEqual(declaration.Retry, expected) {
			return fmt.Errorf("operation %q retry policy does not match compatibility construction %q", operation.ID, registration.Construction)
		}
	}
	if expected := delegatedMetadataContracts(registration.Construction); len(registration.Metadata) > 0 && !reflect.DeepEqual(registration.Metadata, expected) {
		return fmt.Errorf("metadata contracts do not match compatibility construction %q", registration.Construction)
	}
	if len(registration.RuntimeControls) > 0 {
		if !delegated("runtime") {
			return fmt.Errorf("runtime controls require runtime compatibility delegation")
		}
		var expected []manifest.RuntimeControl
		for _, adapter := range Integrated() {
			if adapter.Construction == registration.Construction {
				expected = adapter.RuntimeControls
				break
			}
		}
		if !reflect.DeepEqual(registration.RuntimeControls, expected) {
			return fmt.Errorf("runtime controls do not match compatibility construction %q", registration.Construction)
		}
	}
	if registration.Usage != nil {
		if registration.Usage.Source != "operation" {
			return fmt.Errorf("usage source %q is unavailable for compatibility construction %q", registration.Usage.Source, registration.Construction)
		}
		if registration.Quota == nil {
			return fmt.Errorf("operation-sourced usage has no compiled quota executor")
		}
		if delegated("usage") && registration.Construction != ConstructionCodex {
			return fmt.Errorf("operation-sourced usage delegation is unavailable for compatibility construction %q", registration.Construction)
		}
	}
	return nil
}

func validateDelegatedCompactionOperation(registration Registration, inference *providertransport.Operation) error {
	policy := inference.Compaction
	if policy == nil || policy.Mode != "remote-operation" {
		return nil
	}
	if registration.Construction != ConstructionCodex {
		return fmt.Errorf("operation %q remote compaction is unavailable for compatibility construction %q", inference.ID, registration.Construction)
	}
	operation, ok := registration.Operations[policy.Operation]
	if !ok || operation == nil {
		return fmt.Errorf("operation %q remote compaction references missing operation %q", inference.ID, policy.Operation)
	}
	if operation.Kind != "compaction" {
		return fmt.Errorf("operation %q remote compaction reference %q has kind %q", inference.ID, policy.Operation, operation.Kind)
	}
	if err := operation.ValidateSelection(string(ConstructionOpenAIResponses), "websocket-json"); err != nil {
		return fmt.Errorf("operation %q remote compaction: %w", inference.ID, err)
	}
	if operation.Method != http.MethodPost {
		return fmt.Errorf("operation %q remote compaction method %q is unavailable", operation.ID, operation.Method)
	}
	if operation.Path != "/" {
		return fmt.Errorf("operation %q remote compaction path %q is unavailable", operation.ID, operation.Path)
	}
	if !reflect.DeepEqual(operation.Endpoint, inference.Endpoint) {
		return fmt.Errorf("operation %q remote compaction endpoint does not match the executing Codex endpoint", operation.ID)
	}
	if len(operation.Headers) > 0 {
		return fmt.Errorf("operation %q remote compaction headers are unavailable on the shared Codex WebSocket", operation.ID)
	}
	if operation.ClientIdentity != nil {
		return fmt.Errorf("operation %q remote compaction client identity is unavailable", operation.ID)
	}
	if operation.RequestTransform != nil || operation.ResponseTransform != nil || operation.PromptTransform != nil {
		return fmt.Errorf("operation %q remote compaction transforms are unavailable", operation.ID)
	}
	if operation.RoleMap != nil || operation.ToolCodec != nil {
		return fmt.Errorf("operation %q remote compaction role or tool declarations are unavailable", operation.ID)
	}
	if operation.Anthropic != nil || operation.SystemInstruction != nil {
		return fmt.Errorf("operation %q remote compaction Anthropic declarations are unavailable", operation.ID)
	}
	if operation.Streaming != nil {
		expected := delegatedStreamingPolicy(ConstructionCodex)
		expected.MaxEventBytes = operation.Streaming.MaxEventBytes
		if operation.Streaming.MaxEventBytes <= 0 || !reflect.DeepEqual(operation.Streaming, expected) {
			return fmt.Errorf("operation %q remote compaction streaming declaration does not match the fixed Codex parser", operation.ID)
		}
	}
	if operation.Continuation != nil || operation.Compaction != nil {
		return fmt.Errorf("operation %q remote compaction lifecycle declaration is unavailable", operation.ID)
	}
	if operation.Retry.RetryAfter || len(operation.Retry.Codes) > 0 {
		return fmt.Errorf("operation %q remote compaction retry codes or Retry-After policy are unavailable", operation.ID)
	}
	expectedMetadata := delegatedMetadataContracts(ConstructionCodex)
	if !reflect.DeepEqual(registration.Metadata, expectedMetadata) {
		return fmt.Errorf("operation %q remote compaction metadata contract does not match the Codex executor", operation.ID)
	}
	return nil
}

func delegatedStreamingPolicy(construction Construction) *manifest.StreamingPolicy {
	var mappings []manifest.EventMapping
	eventSource := ""
	switch construction {
	case ConstructionCodex:
		eventSource = "websocket-json"
		mappings = []manifest.EventMapping{
			{Source: "response.output_text.delta", Event: "text-delta"},
			{Source: "response.reasoning_summary_text.delta", Event: "reasoning-delta", MetadataNamespace: "codex"},
			{Source: "response.function_call_arguments.delta", Event: "tool-input-delta"},
			{Source: "response.output_item.done", Event: "tool-call", MetadataNamespace: "codex"},
			{Source: "response.completed", Event: "finish"},
			{Source: "response.incomplete", Event: "error"},
			{Source: "response.failed", Event: "error"},
			{Source: "error", Event: "error"},
		}
	case ConstructionGeminiAntigravity:
		eventSource = "sse-data-json"
		mappings = []manifest.EventMapping{
			{Source: "candidate.content.text", Event: "text-delta"},
			{Source: "candidate.content.thought", Event: "reasoning-delta", MetadataNamespace: "antigravity"},
			{Source: "candidate.content.functionCall", Event: "tool-call", MetadataNamespace: "antigravity"},
			{Source: "usageMetadata", Event: "usage"},
			{Source: "finishReason", Event: "finish"},
			{Source: "error", Event: "error"},
		}
	default:
		return nil
	}
	return &manifest.StreamingPolicy{
		EventSource:      eventSource,
		DoneMarker:       "[DONE]",
		EventTypePointer: "/type",
		Mappings:         mappings,
		RequireTerminal:  true,
		UnknownEvent:     "warn",
		MaxEventBytes:    1024 * 1024,
	}
}

func delegatedRetryPolicy(construction Construction) *manifest.RetryPolicy {
	switch construction {
	case ConstructionCodex:
		return &manifest.RetryPolicy{
			MaxAttempts: 2, Factor: 1, Statuses: []int{}, Codes: []string{},
			TransportErrors: true, UnexpectedEOF: true,
			Authentication: "refresh-once", ReplayRequirement: "before-first-event",
		}
	case ConstructionGeminiAntigravity:
		return &manifest.RetryPolicy{
			MaxAttempts: 7, InitialDelayMS: 2000, MaxDelayMS: 30000, Factor: 2,
			Statuses: []int{http.StatusTooManyRequests, http.StatusServiceUnavailable}, Codes: []string{},
			TransportErrors: true, UnexpectedEOF: true,
			Authentication: "refresh-once", ReplayRequirement: "before-first-event",
		}
	default:
		return nil
	}
}

func retryPoliciesEqual(actual, expected *manifest.RetryPolicy) bool {
	if actual == nil || expected == nil {
		return actual == expected
	}
	return actual.MaxAttempts == expected.MaxAttempts &&
		actual.InitialDelayMS == expected.InitialDelayMS &&
		actual.MaxDelayMS == expected.MaxDelayMS &&
		actual.Factor == expected.Factor &&
		actual.Jitter == expected.Jitter &&
		actual.RetryAfter == expected.RetryAfter &&
		slices.Equal(actual.Statuses, expected.Statuses) &&
		slices.Equal(actual.Codes, expected.Codes) &&
		actual.TransportErrors == expected.TransportErrors &&
		actual.UnexpectedEOF == expected.UnexpectedEOF &&
		actual.Authentication == expected.Authentication &&
		actual.ReplayRequirement == expected.ReplayRequirement
}

func delegatedContinuationPolicy(construction Construction) *manifest.ContinuationPolicy {
	switch construction {
	case ConstructionCodex:
		return &manifest.ContinuationPolicy{
			Mode: "previous-response", ResponseIDPointer: "/response/id", RequestField: "previous_response_id",
			RequiredStableFields: []string{"model", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "include", "text", "prompt_cache_key", "store"},
			AppendOnlyHistory:    true, Store: "optional", Fallback: "full-replay", MaxIdleSeconds: 1800, MetadataNamespace: "codex",
		}
	case ConstructionGeminiAntigravity:
		return &manifest.ContinuationPolicy{Mode: "none", Store: "forbidden", Fallback: "full-replay"}
	default:
		return nil
	}
}

func delegatedCompactionPolicy(construction Construction) *manifest.CompactionPolicy {
	switch construction {
	case ConstructionCodex:
		return &manifest.CompactionPolicy{
			Mode: "remote-operation", Operation: "remote-compact", RetainedTokenBudget: 64000,
			PreserveToolPairs: true, MetadataNamespace: "codex",
		}
	case ConstructionGeminiAntigravity:
		return &manifest.CompactionPolicy{Mode: "local-summary"}
	default:
		return nil
	}
}

func delegatedMetadataContracts(construction Construction) []manifest.MetadataContract {
	schema := func() map[string]any {
		return map[string]any{
			"$schema":              "https://json-schema.org/draft/2020-12/schema",
			"additionalProperties": true,
			"type":                 "object",
		}
	}
	switch construction {
	case ConstructionCodex:
		return []manifest.MetadataContract{
			{Namespace: "codex", Version: 1, Scope: "reasoning", Schema: schema(), RequiredForReplay: true},
			{Namespace: "codex", Version: 1, Scope: "tool-call", Schema: schema(), RequiredForReplay: true},
			{Namespace: "codex", Version: 1, Scope: "continuation", Schema: schema()},
			{Namespace: "codex", Version: 1, Scope: "compaction", Schema: schema(), RequiredForReplay: true},
		}
	case ConstructionGeminiAntigravity:
		return []manifest.MetadataContract{
			{Namespace: "antigravity", Version: 1, Scope: "reasoning", Schema: schema(), RequiredForReplay: true},
			{Namespace: "google", Version: 1, Scope: "reasoning", Schema: schema(), LegacyProjection: "gemini-thought-signature"},
		}
	default:
		return nil
	}
}

func geminiRoleMapMismatches(mapping *manifest.RoleMap) []string {
	if mapping == nil {
		return []string{"missing"}
	}
	expected := []struct {
		name, actual, value string
	}{
		{name: "system", actual: mapping.System, value: "system"},
		{name: "developer", actual: mapping.Developer, value: "system"},
		{name: "user", actual: mapping.User, value: "user"},
		{name: "assistant", actual: mapping.Assistant, value: "model"},
		{name: "tool", actual: mapping.Tool, value: "user"},
		{name: "unknown", actual: mapping.Unknown, value: "reject"},
	}
	var result []string
	for _, field := range expected {
		if field.actual != field.value {
			result = append(result, field.name)
		}
	}
	return result
}

func geminiResponseTransformEquivalent(pipeline *manifest.JSONPipeline) bool {
	if pipeline == nil || len(pipeline.Operations) == 0 || pipeline.MaxOperations > 0 && len(pipeline.Operations) > pipeline.MaxOperations {
		return false
	}
	allowed := map[string]string{
		"/response/candidates":    "/candidates",
		"/response/usageMetadata": "/usageMetadata",
		"/response/error":         "/error",
	}
	for _, operation := range pipeline.Operations {
		switch operation.Operation {
		case "copy", "move":
			if allowed[operation.From] != operation.Path || operation.Value != nil || len(operation.Keys) > 0 || operation.Predicate != nil {
				return false
			}
		case "delete":
			if operation.Path != "/response" || operation.From != "" || operation.Value != nil || len(operation.Keys) > 0 || operation.Predicate != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func declaredInferenceOperation(registration Registration) *manifest.Operation {
	if registration.Manifest == nil || registration.Operation == nil {
		return nil
	}
	for i := range registration.Manifest.Capabilities.Operations {
		operation := &registration.Manifest.Capabilities.Operations[i]
		if operation.Kind == "inference" && operation.ID == registration.Operation.ID {
			return operation
		}
	}
	return nil
}

func constructionDelegated(registration Registration) bool {
	compatibility := registration.Manifest.Capabilities.Compatibility
	return compatibility != nil && slices.Contains(compatibility.Delegates, "construction")
}

func validateNativeTransport(construction Construction, operation *providertransport.Operation) error {
	var transports []string
	switch construction {
	case ConstructionAnthropicMessages, ConstructionOpenAIResponses:
		transports = []string{"sse"}
	case ConstructionGeminiContent, ConstructionGeminiInteraction, ConstructionGenericJSON:
		transports = []string{"http-json", "sse"}
	default:
		return fmt.Errorf("unsupported plugin-native construction %q", construction)
	}
	return operation.ValidateSelection(string(construction), transports...)
}

func validateDeclarativeFraming(operation *providertransport.Operation) error {
	switch operation.Key.Transport {
	case "http-json":
		if operation.Streaming != nil {
			return fmt.Errorf("operation %q http-json transport must not declare streaming policy", operation.ID)
		}
	case "sse":
		if operation.Streaming == nil {
			return fmt.Errorf("operation %q SSE transport requires a streaming policy using sse-data-json", operation.ID)
		}
		if operation.Streaming.EventSource != "sse-data-json" {
			return fmt.Errorf("operation %q SSE transport requires sse-data-json event source, not %q", operation.ID, operation.Streaming.EventSource)
		}
		if len(operation.Streaming.Mappings) > 0 && operation.Streaming.EventTypePointer == "" {
			return fmt.Errorf("operation %q SSE event mappings require an event type pointer", operation.ID)
		}
		if operation.Streaming.EventTypePointer == "" || len(operation.Streaming.EventTypePointer) > 1024 || !manifest.ValidJSONPointer(operation.Streaming.EventTypePointer) {
			return fmt.Errorf("operation %q SSE event type pointer is not a valid non-root JSON Pointer", operation.ID)
		}
		hasFinish := false
		if err := manifest.ValidateEventMappings(operation.Streaming.Mappings); err != nil {
			return fmt.Errorf("operation %q SSE event mappings: %w", operation.ID, err)
		}
		for _, mapping := range operation.Streaming.Mappings {
			if mapping.Event == "finish" {
				hasFinish = true
			}
		}
		if !hasFinish {
			return fmt.Errorf("operation %q SSE transport requires a mapped finish event", operation.ID)
		}
	}
	return nil
}

func validateNativeLifecycle(construction Construction, operation *providertransport.Operation) error {
	if construction != ConstructionOpenAIResponses {
		if operation.Retry.Authentication != "never" {
			return fmt.Errorf("operation %q retry authentication %q has no native refresh callback", operation.ID, operation.Retry.Authentication)
		}
		if operation.Retry.UnexpectedEOF {
			return fmt.Errorf("operation %q unexpected-EOF retry cannot be enforced before streamed events are consumed", operation.ID)
		}
		if operation.Continuation != nil && operation.Continuation.Mode != "none" {
			return fmt.Errorf("operation %q continuation mode %q has no conversation-scoped native state bridge", operation.ID, operation.Continuation.Mode)
		}
	}
	if operation.Retry.MaxAttempts > 1 && operation.Retry.ReplayRequirement == "never" {
		return fmt.Errorf("operation %q requests retries but forbids request replay", operation.ID)
	}
	if construction == ConstructionOpenAIResponses {
		if len(operation.Retry.Codes) > 0 {
			return fmt.Errorf("operation %q retry codes are unavailable for native OpenAI Responses construction", operation.ID)
		}
		if operation.Retry.Authentication == "refresh-once" {
			return fmt.Errorf("operation %q refresh-once authentication is unavailable for the complete native OpenAI Responses language-model contract", operation.ID)
		}
		if operation.Retry.MaxAttempts > 1 && operation.Retry.UnexpectedEOF && (len(operation.Retry.Statuses) > 0 || operation.Retry.TransportErrors) {
			return fmt.Errorf("operation %q cannot share one max-attempts budget across HTTP and unexpected-EOF retries", operation.ID)
		}
		if err := validateOpenAIResponsesContinuation(operation); err != nil {
			return err
		}
	}
	if operation.Compaction != nil {
		switch operation.Compaction.Mode {
		case "none":
		case "local-summary":
			if operation.Compaction.Operation != "" {
				return fmt.Errorf("operation %q local-summary compaction operation %q is not executed", operation.ID, operation.Compaction.Operation)
			}
			if operation.Compaction.PreserveToolPairs {
				return fmt.Errorf("operation %q local-summary compaction tool-pair preservation is not executed", operation.ID)
			}
			if operation.Compaction.MetadataNamespace != "" {
				return fmt.Errorf("operation %q local-summary compaction metadata namespace %q is not executed", operation.ID, operation.Compaction.MetadataNamespace)
			}
		default:
			return fmt.Errorf("operation %q compaction mode %q is not executed by its native constructor", operation.ID, operation.Compaction.Mode)
		}
	}
	return nil
}

func validateOpenAIResponsesContinuation(operation *providertransport.Operation) error {
	policy := operation.Continuation
	if policy == nil || policy.Mode == "none" {
		return nil
	}
	if policy.Mode != "previous-response" {
		return fmt.Errorf("operation %q continuation mode %q is unavailable for native OpenAI Responses construction", operation.ID, policy.Mode)
	}
	if policy.ResponseIDPointer != "/id" {
		return fmt.Errorf("operation %q previous-response response ID pointer %q is unavailable; native OpenAI Responses requires /id", operation.ID, policy.ResponseIDPointer)
	}
	if policy.RequestField != "previous_response_id" {
		return fmt.Errorf("operation %q previous-response request field %q is unavailable; native OpenAI Responses requires previous_response_id", operation.ID, policy.RequestField)
	}
	if !policy.AppendOnlyHistory {
		return fmt.Errorf("operation %q previous-response requires append-only history in native OpenAI Responses construction", operation.ID)
	}
	if policy.MaxIdleSeconds > 0 {
		return fmt.Errorf("operation %q previous-response idle expiry is unavailable for native OpenAI Responses construction", operation.ID)
	}
	for _, field := range policy.RequiredStableFields {
		if field != "model" && field != "instructions" && field != "tools" {
			return fmt.Errorf("operation %q previous-response stable field %q is unavailable for native OpenAI Responses construction", operation.ID, field)
		}
	}
	return nil
}

func validateOpenAIResponsesMetadata(operation *providertransport.Operation, contracts []manifest.MetadataContract) error {
	namespace := ""
	if operation.Continuation != nil {
		namespace = operation.Continuation.MetadataNamespace
	}
	if namespace == "" && len(contracts) == 0 {
		return nil
	}
	if namespace == "" || len(contracts) != 1 {
		return fmt.Errorf("continuation metadata contract is unavailable for native OpenAI Responses construction")
	}
	contract := contracts[0]
	expectedSchema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"response_id": map[string]any{"type": "string"},
		},
		"required":             []any{"response_id"},
		"additionalProperties": false,
	}
	if contract.Namespace != namespace || contract.Version != 1 || contract.Scope != "continuation" || contract.RequiredForReplay || contract.LegacyProjection != "" || !reflect.DeepEqual(contract.Schema, expectedSchema) {
		return fmt.Errorf("continuation metadata contract is unavailable for native OpenAI Responses construction")
	}
	return nil
}

func activationError(registration Registration, err error) error {
	return fmt.Errorf("activate provider %q: %w", registration.ProviderID, err)
}
