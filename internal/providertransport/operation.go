// Package providertransport compiles provider manifests into immutable,
// provider-neutral operation configuration. It contains no consumer endpoint,
// OAuth registration, official-client identity, quota, or private envelope
// defaults.
package providertransport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

const (
	DefaultConnectTimeout = 30 * time.Second
	DefaultRequestTimeout = 300 * time.Second
	DefaultStreamIdle     = 60 * time.Second
)

type Key struct {
	Protocol  string
	Transport string
}

type ResolvedSystemInstruction struct {
	Text         string
	CacheControl *manifest.AnthropicCacheControl
}

// Operation is a closed immutable projection of one manifest operation and all
// declarative policy it references.
type Operation struct {
	ID                string
	Kind              string
	Key               Key
	Endpoint          manifest.Endpoint
	Method            string
	Path              string
	Headers           []manifest.HeaderRule
	RequestTransform  *manifest.JSONPipeline
	ResponseTransform *manifest.JSONPipeline
	PromptTransform   *manifest.PromptPipeline
	RoleMap           *manifest.RoleMap
	ToolCodec         *manifest.ToolCodec
	ClientIdentity    *manifest.ResolvedClientIdentity
	Anthropic         *manifest.AnthropicPolicy
	SystemInstruction *ResolvedSystemInstruction
	Streaming         *manifest.StreamingPolicy
	Retry             manifest.RetryPolicy
	Errors            []manifest.ErrorMapping
	Continuation      *manifest.ContinuationPolicy
	Compaction        *manifest.CompactionPolicy
	Timeouts          *manifest.TimeoutHints
	ConnectTimeout    time.Duration
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
}

// Compile binds one validated manifest operation to every declarative policy
// needed at runtime. The resulting snapshot is the handoff between plugin
// registration and provider construction. Do not omit policy fields, borrow
// defaults from a built-in provider, or retain mutable references to manifest
// data; any of those changes silently alters active private-plugin behavior.
func Compile(value manifest.Manifest, operation manifest.Operation) (*Operation, error) {
	endpoint, ok := findEndpoint(value.Capabilities.Endpoints, operation.Endpoint)
	if !ok {
		return nil, fmt.Errorf("operation %q references missing endpoint %q", operation.ID, operation.Endpoint)
	}
	headers := clone(operation.Headers)
	if operation.Kind == "inference" {
		headers = append(clone(value.Capabilities.Headers), headers...)
	}
	var errorMappings []manifest.ErrorMapping
	if operation.Kind == "inference" || operation.Kind == "compaction" {
		errorMappings = clone(value.Capabilities.Errors)
	}
	compiled := &Operation{
		ID: operation.ID, Kind: operation.Kind,
		Key:      Key{Protocol: operation.Protocol, Transport: operation.Transport},
		Endpoint: endpoint, Method: operation.Method, Path: operation.Path,
		Headers: headers, Anthropic: clonePointer(value.Capabilities.Anthropic), Streaming: clonePointer(operation.Streaming),
		Errors: errorMappings, Continuation: clonePointer(operation.Continuation), Compaction: clonePointer(operation.Compaction), Timeouts: clonePointer(operation.Timeouts),
		ConnectTimeout: DefaultConnectTimeout, RequestTimeout: DefaultRequestTimeout, StreamIdleTimeout: DefaultStreamIdle,
		Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
	}
	if operation.Retry != nil {
		compiled.Retry = clone(*operation.Retry)
	}
	if operation.Timeouts != nil {
		if operation.Timeouts.ConnectSeconds > 0 {
			compiled.ConnectTimeout = time.Duration(operation.Timeouts.ConnectSeconds) * time.Second
		}
		if operation.Timeouts.RequestSeconds > 0 {
			compiled.RequestTimeout = time.Duration(operation.Timeouts.RequestSeconds) * time.Second
		}
		if operation.Timeouts.IdleSeconds > 0 {
			compiled.StreamIdleTimeout = time.Duration(operation.Timeouts.IdleSeconds) * time.Second
		}
	}
	if operation.RequestTransform != "" {
		pipeline, ok := value.Capabilities.JSONTransforms[operation.RequestTransform]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing request transform %q", operation.ID, operation.RequestTransform)
		}
		compiled.RequestTransform = clonePointer(&pipeline)
	}
	if operation.ResponseTransform != "" {
		pipeline, ok := value.Capabilities.JSONTransforms[operation.ResponseTransform]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing response transform %q", operation.ID, operation.ResponseTransform)
		}
		compiled.ResponseTransform = clonePointer(&pipeline)
	}
	if operation.PromptTransform != "" {
		pipeline, ok := value.Capabilities.PromptTransforms[operation.PromptTransform]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing prompt transform %q", operation.ID, operation.PromptTransform)
		}
		compiled.PromptTransform = clonePointer(&pipeline)
	}
	if operation.RoleMap != "" {
		mapping, ok := value.Capabilities.RoleMaps[operation.RoleMap]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing role map %q", operation.ID, operation.RoleMap)
		}
		compiled.RoleMap = clonePointer(&mapping)
	}
	if operation.ToolCodec != "" {
		codec, ok := value.Capabilities.ToolCodecs[operation.ToolCodec]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing tool codec %q", operation.ID, operation.ToolCodec)
		}
		compiled.ToolCodec = clonePointer(&codec)
	}
	if operation.ClientIdentity != "" {
		identity, ok := value.Capabilities.ClientIdentities[operation.ClientIdentity]
		if !ok {
			return nil, fmt.Errorf("operation %q references missing client identity %q", operation.ID, operation.ClientIdentity)
		}
		compiled.ClientIdentity = clonePointer(&identity)
	}
	return compiled, nil
}

// Clone isolates a compiled operation for a transport instance. Plugin policy
// contains slices, maps, and nested structures that must not be shared with
// mutable registration state or another client.
func (o *Operation) Clone() *Operation {
	if o == nil {
		return nil
	}
	return clonePointer(o)
}

// ResolveEndpoint applies the manifest's endpoint ownership policy. A
// forbidden override always returns the immutable manifest endpoint; allowed
// overrides remain constrained by scheme, host, and same-origin declarations.
// Do not fall back to a generic public endpoint when validation fails.
func (o *Operation) HTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	if o != nil && o.RequestTimeout > 0 {
		client.Timeout = o.RequestTimeout
	}
	if o != nil && !o.Endpoint.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return &client
}

func (o *Operation) ResolveEndpoint(configured string) (string, error) {
	if o == nil {
		return configured, nil
	}
	declared, err := url.Parse(o.Endpoint.BaseURL)
	if err != nil {
		return "", fmt.Errorf("operation %q has invalid endpoint: %w", o.ID, err)
	}
	configured = strings.TrimSpace(configured)
	if configured == "" || sameURL(configured, o.Endpoint.BaseURL) {
		return o.Endpoint.BaseURL, nil
	}
	if o.Endpoint.Override == "forbidden" {
		return "", fmt.Errorf("operation %q endpoint override is forbidden", o.ID)
	}
	candidate, err := url.Parse(configured)
	if err != nil || candidate.Hostname() == "" {
		return "", fmt.Errorf("operation %q endpoint override is invalid", o.ID)
	}
	if !containsFold(o.Endpoint.AllowedSchemes, candidate.Scheme) || !containsFold(o.Endpoint.AllowedHosts, candidate.Hostname()) {
		return "", fmt.Errorf("operation %q endpoint override violates its origin allowlist", o.ID)
	}
	if o.Endpoint.Override == "same-origin" && (!strings.EqualFold(candidate.Scheme, declared.Scheme) || !strings.EqualFold(candidate.Hostname(), declared.Hostname()) || candidate.Port() != declared.Port()) {
		return "", fmt.Errorf("operation %q endpoint override must use the declared origin", o.ID)
	}
	return candidate.String(), nil
}

// ApplyHeaders applies ordered manifest rules after configured headers. This
// ordering gives the trusted plugin contract final ownership of protected
// identity and protocol headers. Do not reverse the order, skip delete rules,
// or let ambient provider headers replace protected values.
func (o *Operation) ApplyHeaders(configured map[string]string, contextValues map[string]string) (map[string]string, error) {
	if o == nil {
		return configured, nil
	}
	headers := make(http.Header, len(configured))
	for name, value := range configured {
		headers.Set(name, value)
	}
	for _, rule := range o.Headers {
		if rule.Operation == "delete" {
			headers.Del(rule.Name)
			continue
		}
		if rule.Value == nil {
			return nil, fmt.Errorf("operation %q header %q has no value", o.ID, rule.Name)
		}
		value, err := operationTemplate(*rule.Value, contextValues)
		if err != nil {
			return nil, fmt.Errorf("operation %q header %q: %w", o.ID, rule.Name, err)
		}
		switch rule.Operation {
		case "set":
			headers.Set(rule.Name, value)
		case "set-if-absent":
			if headers.Get(rule.Name) == "" {
				headers.Set(rule.Name, value)
			}
		case "append":
			headers.Add(rule.Name, value)
		case "append-unique":
			found := false
			for _, current := range strings.Split(headers.Get(rule.Name), ",") {
				if strings.TrimSpace(current) == value {
					found = true
					break
				}
			}
			if !found {
				headers.Add(rule.Name, value)
			}
		default:
			return nil, fmt.Errorf("operation %q uses unsupported header operation %q", o.ID, rule.Operation)
		}
	}
	result := make(map[string]string, len(headers))
	for name, values := range headers {
		result[name] = strings.Join(values, ",")
	}
	return result, nil
}

// operationTemplate evaluates only the bounded template forms allowed in
// compiled header policy. Extending it with environment, file, or command
// access would turn a declarative plugin into an executable trust boundary.
func operationTemplate(value manifest.Template, contextValues map[string]string) (string, error) {
	switch value.Kind {
	case "literal":
		if text, ok := value.Value.(string); ok {
			return text, nil
		}
		encoded, err := json.Marshal(value.Value)
		return string(encoded), err
	case "context":
		return contextValues[value.Ref], nil
	case "concat":
		var result strings.Builder
		for _, part := range value.Parts {
			text, err := operationTemplate(part, contextValues)
			if err != nil {
				return "", err
			}
			result.WriteString(text)
		}
		return result.String(), nil
	default:
		return "", fmt.Errorf("template kind %q is unavailable in operation headers", value.Kind)
	}
}

// sameURL performs the exact normalized comparison used before endpoint
// override enforcement. Keep it credential-blind and fail closed on parse
// errors so malformed configuration cannot bypass manifest endpoint policy.
func sameURL(left, right string) bool {
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	return leftErr == nil && rightErr == nil && leftURL.String() == rightURL.String()
}

// containsFold implements case-insensitive matching for schemes, hosts, and
// related HTTP policy values. Case-sensitive replacement would reject valid
// declarations or create inconsistent allowlist behavior.
func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// ValidateSelection prevents a consumer adapter from silently replacing a
// manifest's protocol or transport with a nearby implementation. A matching
// provider name is not sufficient: protocol and transport determine framing,
// identity, retries, and stream semantics. A nil operation represents an
// integrated compatibility registration and is left to its explicit
// constructor.
func (o *Operation) ValidateSelection(protocol string, transports ...string) error {
	if o == nil {
		return nil
	}
	if o.Key.Protocol != protocol {
		return fmt.Errorf("operation %q declares protocol %q, constructor requires %q", o.ID, o.Key.Protocol, protocol)
	}
	for _, transport := range transports {
		if o.Key.Transport == transport {
			return nil
		}
	}
	return fmt.Errorf("operation %q declares unsupported %s transport %q", o.ID, protocol, o.Key.Transport)
}

// findEndpoint returns a detached copy of the exact endpoint referenced by an
// operation. Do not select the first endpoint or a host default when the ID is
// missing; Compile must reject an incomplete plugin contract.
func findEndpoint(endpoints []manifest.Endpoint, id string) (manifest.Endpoint, bool) {
	for _, endpoint := range endpoints {
		if endpoint.ID == id {
			return clone(endpoint), true
		}
	}
	return manifest.Endpoint{}, false
}

// clonePointer deep-copies optional manifest policy so compiled operations and
// transports cannot mutate registration-owned data through shared pointers.
func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	result := clone(*value)
	return &result
}

// clone deep-copies declarative manifest values through their JSON contract.
// On failure it preserves the original value rather than inventing partial
// policy; callers validate manifests before compilation.
func clone[T any](value T) T {
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
