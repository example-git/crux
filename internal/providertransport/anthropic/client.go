// Package anthropic provides finite host-interpreted Anthropic Messages wire
// policy. All provider identity, header, prompt, metadata, and tool alias values
// come from a trusted manifest operation.
package anthropic

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/providertransport/clientidentity"
)

var processValues sync.Map

type Client struct {
	operation *providertransport.Operation
	version   string
	userAgent string
	sessionID string
	base      http.RoundTripper
}

// NewClient is the only valid constructor for a manifest-backed Anthropic
// transport. It resolves the manifest-declared version and user agent, creates
// a process-stable session identity, and clones the complete wire policy. Do
// not replace this client with a generic HTTP transport: requests may still
// reach the endpoint while silently losing required identity and transforms.
func NewClient(operation *providertransport.Operation, debug bool, validate providertransport.OwnerValidator) (*http.Client, error) {
	if operation == nil || operation.Anthropic == nil {
		return nil, fmt.Errorf("Anthropic operation has no wire policy")
	}
	if validate == nil {
		return nil, fmt.Errorf("Anthropic provider owner validator is unavailable")
	}
	policy := operation.Anthropic
	version, userAgent, err := ResolveIdentity(policy.ClientIdentity)
	if err != nil {
		return nil, err
	}
	sessionKey := "anthropic:" + operation.Endpoint.ID
	session, _ := processValues.LoadOrStore(sessionKey, newUUID())
	base := http.DefaultTransport
	if debug {
		base = log.NewHTTPClient().Transport
	}
	base = providertransport.TransportWithConnectTimeout(base, operation.ConnectTimeout)
	base = providertransport.TransportWithStreamIdleTimeout(base, operation.StreamIdleTimeout)
	client := &http.Client{Transport: &Client{operation: operation.Clone(), version: version, userAgent: userAgent, sessionID: session.(string), base: providertransport.OwnerValidatingTransport(base, validate)}}
	return operation.HTTPClient(client), nil
}

// EffectiveBaseURL enforces the endpoint override policy compiled from the
// plugin manifest. It must fail on forbidden or unknown policy instead of
// quietly using a configured or generic endpoint, which could send plugin
// credentials and identity headers to the wrong origin.
func EffectiveBaseURL(operation *providertransport.Operation, configured string) (string, error) {
	if operation == nil {
		return configured, nil
	}
	declared := operation.Endpoint.BaseURL
	switch operation.Endpoint.Override {
	case "forbidden":
		if configured != "" && strings.TrimRight(configured, "/") != strings.TrimRight(declared, "/") {
			return "", fmt.Errorf("endpoint override is forbidden")
		}
		return declared, nil
	case "same-origin", "allowed-hosts":
		if configured != "" {
			return configured, nil
		}
		return declared, nil
	default:
		return "", fmt.Errorf("unsupported endpoint override policy %q", operation.Endpoint.Override)
	}
}

// RoundTrip applies the complete manifest wire contract to every request and
// response. The order is intentional: remove forbidden ambient headers, apply
// protected identity rules including User-Agent, restore allowed unique caller
// values, set session identity, rewrite the bounded body, then remap streamed
// tool names. Bypassing or reordering these stages breaks plugin behavior while
// leaving ordinary HTTP health checks deceptively successful.
func (c *Client) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("HTTP request is nil")
	}
	request = request.Clone(request.Context())
	policy := c.operation.Anthropic
	for name := range request.Header {
		lower := strings.ToLower(name)
		if containsFold(policy.DeleteHeaders, lower) || hasPrefixFold(lower, policy.DeleteHeaderPrefixes) {
			request.Header.Del(name)
		}
	}
	contexts := map[string]string{"client.version": c.version, "client.user_agent": c.userAgent, "session.id": c.sessionID}
	preserved := map[string][]string{}
	for _, rule := range c.operation.Headers {
		if rule.Protected && rule.Operation == "append-unique" {
			canonical := http.CanonicalHeaderKey(rule.Name)
			if _, ok := preserved[canonical]; !ok {
				preserved[canonical] = splitHeader(request.Header.Get(rule.Name))
				request.Header.Del(rule.Name)
			}
		}
	}
	for _, rule := range c.operation.Headers {
		if err := applyHeader(request.Header, rule, contexts); err != nil {
			return nil, err
		}
	}
	for name, values := range preserved {
		current := splitHeader(request.Header.Get(name))
		for _, value := range values {
			if !containsFold(current, value) {
				current = append(current, value)
			}
		}
		request.Header.Set(name, strings.Join(current, ","))
	}
	if policy.SessionHeader != "" {
		request.Header.Set(policy.SessionHeader, c.sessionID)
	}
	if request.Body != nil && request.Body != http.NoBody {
		body, err := readBounded(request.Body, policy.MaxRequestBytes)
		_ = request.Body.Close()
		if err != nil {
			return nil, err
		}
		rewritten, rewriteErr := rewriteBody(body, c.operation, c.version, c.sessionID)
		if rewriteErr != nil {
			if policy.TransformFailure != "warn-original" {
				return nil, rewriteErr
			}
			slog.Warn("Provider request transform failed; sending original body", "error", rewriteErr)
			rewritten = body
		}
		request.Body = io.NopCloser(bytes.NewReader(rewritten))
		request.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		request.ContentLength = int64(len(rewritten))
	}
	response, err := providertransport.RoundTripWithRetry(request, c.base, c.operation.Retry, c.operation.Errors)
	if err != nil {
		return response, err
	}
	if response.Body != nil && c.operation.ToolCodec != nil && hasSurface(c.operation.ToolCodec.Surfaces, "stream-events") {
		holdback := policy.StreamHoldbackBytes
		if holdback <= 0 {
			holdback = 256
		}
		response.Body = &remapReader{reader: response.Body, codec: c.operation.ToolCodec, holdback: holdback}
	}
	return response, nil
}

// rewriteBody applies the plugin's request policy as one atomic JSON rewrite.
// It preserves unrelated content, including images, while applying declared
// deletions, prompt filtering, tool aliases, billing attribution, and metadata
// identity. Do not split these operations across generic SDK hooks, where order
// and non-text content preservation can diverge.
func rewriteBody(body []byte, operation *providertransport.Operation, version, session string) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	if operation.RequestTransform != nil {
		for _, item := range operation.RequestTransform.Operations {
			if item.Operation == "delete" {
				deletePointer(document, item.Path)
			}
		}
	}
	policy := operation.Anthropic
	instructionSelected := false
	if operation.SystemInstruction != nil {
		instructionSelected = extractSystemInstruction(document, operation.SystemInstruction.Text)
	}
	for _, prefix := range policy.SystemLinePrefixes {
		filterSystemLines(document, prefix)
	}
	if operation.ToolCodec != nil {
		remapTools(document, operation.ToolCodec)
		remapReferences(document, operation.ToolCodec)
	}
	blocks := make([]manifest.AnthropicSystemBlock, 0, len(policy.SystemBlocks)+len(policy.SystemPrefixes)+1)
	if policy.Billing != nil {
		blocks = append(blocks, manifest.AnthropicSystemBlock{Text: billing(document, *policy.Billing, version)})
	}
	blocks = append(blocks, policy.SystemBlocks...)
	for _, prefix := range policy.SystemPrefixes {
		blocks = append(blocks, manifest.AnthropicSystemBlock{Text: prefix})
	}
	if len(blocks) > 0 || instructionSelected {
		prependSystem(document, blocks, operation.SystemInstruction, instructionSelected)
	}
	if policy.MetadataUserID {
		metadata, _ := document["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		encoded, _ := json.Marshal(map[string]string{"device_id": session, "account_uuid": "", "session_id": session})
		metadata["user_id"] = string(encoded)
		document["metadata"] = metadata
	}
	return json.Marshal(document)
}

// remapTools converts host tool names and parameters to the provider dialect
// across definitions and conversation history according to declared surfaces.
// Definitions, calls, and results must remain consistent or the model will emit
// names the host cannot execute.
func remapTools(document map[string]any, codec *manifest.ToolCodec) {
	params := map[string]map[string]string{}
	for _, mapping := range codec.Parameters {
		if params[mapping.Tool] == nil {
			params[mapping.Tool] = map[string]string{}
		}
		params[mapping.Tool][mapping.Host] = mapping.Provider
	}
	if hasSurface(codec.Surfaces, "definitions") {
		tools := array(document["tools"])
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			host, _ := tool["name"].(string)
			provider, prefix, found := outboundToolName(host, codec)
			if !found {
				continue
			}
			tool["name"] = provider
			remapSchema(tool, params[host])
			applyPrefixDefinitionPolicy(tool, prefix)
		}
		if codec.ToolSearch != "" && !hasToolSearchDefinition(tools) {
			tools = append(tools, toolSearchDefinition(codec.ToolSearch))
			document["tools"] = tools
		}
	}
	for _, rawMessage := range array(document["messages"]) {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		for _, rawBlock := range array(message["content"]) {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			host, _ := block["name"].(string)
			provider, _, found := outboundToolName(host, codec)
			if !found {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if hasSurface(codec.Surfaces, "history-calls") {
					block["name"] = provider
					remapInput(block, params[host])
				}
			case "tool_result":
				if hasSurface(codec.Surfaces, "history-results") {
					block["name"] = provider
				}
			}
		}
	}
}

func outboundToolName(host string, codec *manifest.ToolCodec) (string, *manifest.ToolPrefixAlias, bool) {
	for _, alias := range codec.Aliases {
		if alias.Host == host {
			return alias.Provider, nil, true
		}
	}
	for index := range codec.PrefixAliases {
		alias := &codec.PrefixAliases[index]
		if strings.HasPrefix(host, alias.HostPrefix) {
			return alias.ProviderPrefix + strings.TrimPrefix(host, alias.HostPrefix), alias, true
		}
	}
	return "", nil, false
}

func inboundToolName(provider string, codec *manifest.ToolCodec) (string, bool) {
	key := provider
	if codec.CaseFoldInbound {
		key = strings.ToLower(key)
	}
	for _, alias := range codec.Aliases {
		candidate := alias.Provider
		if codec.CaseFoldInbound {
			candidate = strings.ToLower(candidate)
		}
		if candidate == key {
			return alias.Host, true
		}
	}
	for _, alias := range codec.PrefixAliases {
		prefix := alias.ProviderPrefix
		if codec.CaseFoldInbound {
			prefix = strings.ToLower(prefix)
		}
		if strings.HasPrefix(key, prefix) {
			return alias.HostPrefix + provider[len(alias.ProviderPrefix):], true
		}
	}
	return "", false
}

func applyPrefixDefinitionPolicy(tool map[string]any, prefix *manifest.ToolPrefixAlias) {
	if prefix == nil {
		return
	}
	if prefix.DeferLoading {
		tool["defer_loading"] = true
	}
	if !prefix.OmitEmptyRequired {
		return
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema == nil {
		return
	}
	required, exists := schema["required"]
	if !exists {
		return
	}
	switch values := required.(type) {
	case []any:
		if len(values) == 0 {
			delete(schema, "required")
		}
	case []string:
		if len(values) == 0 {
			delete(schema, "required")
		}
	}
}

func hasToolSearchDefinition(tools []any) bool {
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		if strings.HasPrefix(kind, "tool_search_tool_") {
			return true
		}
	}
	return false
}

func toolSearchDefinition(kind string) map[string]any {
	name := "tool_search_tool_" + kind
	return map[string]any{
		"name": name,
		"type": name + "_20251119",
	}
}

// remapSchema keeps a tool's property map and required list synchronized while
// renaming parameters. Changing only one side produces schemas that advertise
// arguments the provider or host cannot satisfy.
func remapSchema(tool map[string]any, renames map[string]string) {
	schema, _ := tool["input_schema"].(map[string]any)
	if schema == nil {
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	for from, to := range renames {
		if value, ok := properties[from]; ok {
			properties[to] = value
			delete(properties, from)
		}
	}
	for index, raw := range array(schema["required"]) {
		if name, ok := raw.(string); ok {
			if to, found := renames[name]; found {
				schema["required"].([]any)[index] = to
			}
		}
	}
}

// remapInput applies the same declared parameter aliases to historical tool
// calls. This must match remapSchema so retries and continued conversations use
// one stable provider dialect.
func remapInput(block map[string]any, renames map[string]string) {
	input, _ := block["input"].(map[string]any)
	for from, to := range renames {
		if value, ok := input[from]; ok {
			input[to] = value
			delete(input, from)
		}
	}
}

// remapReferences updates prompt and tool-description references only when the
// manifest declares that surface. Unconditional replacement can corrupt user
// text; skipping declared replacement teaches the model unusable host names.
func remapReferences(document map[string]any, codec *manifest.ToolCodec) {
	if !hasSurface(codec.Surfaces, "prompt-references") {
		return
	}
	pairs := make([]string, 0, len(codec.Aliases)*2)
	for _, alias := range codec.Aliases {
		pairs = append(pairs, "`"+alias.Host+"`", "`"+alias.Provider+"`")
	}
	replacer := strings.NewReplacer(pairs...)
	transform := func(value string) string {
		value = replacer.Replace(value)
		for _, alias := range codec.PrefixAliases {
			value = strings.ReplaceAll(value, "`"+alias.HostPrefix, "`"+alias.ProviderPrefix)
		}
		return value
	}
	mapSystemText(document, transform)
	for _, raw := range array(document["tools"]) {
		if tool, ok := raw.(map[string]any); ok {
			if text, ok := tool["description"].(string); ok {
				tool["description"] = transform(text)
			}
		}
	}
}

// filterSystemLines removes only manifest-declared host prompt lines while
// preserving line endings and all unrelated system content. Broad filtering or
// generic prompt replacement can delete user instructions required by plugins.
func filterSystemLines(document map[string]any, prefix string) {
	mapSystemText(document, func(value string) string {
		lines := strings.SplitAfter(value, "\n")
		var result strings.Builder
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), prefix) {
				continue
			}
			result.WriteString(line)
		}
		return result.String()
	})
}

// mapSystemText applies a transform to both supported Anthropic system forms:
// a string or text blocks. Keep both paths so plugin policy does not depend on
// which equivalent representation the upstream SDK emitted.
func mapSystemText(document map[string]any, transform func(string) string) {
	switch system := document["system"].(type) {
	case string:
		document["system"] = transform(system)
	case []any:
		for _, raw := range system {
			if block, ok := raw.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					block["text"] = transform(text)
				}
			}
		}
	}
}

// prependSystem installs manifest-owned system prefixes exactly once and ahead
// of existing content. Duplicate attribution or identity prefixes can change
// request semantics and billing fingerprints, while appending them may violate
// the provider's expected ordering.
func prependSystem(document map[string]any, prefixes []manifest.AnthropicSystemBlock, instruction *providertransport.ResolvedSystemInstruction, instructionSelected bool) {
	existing := normalizedSystemBlocks(document["system"])
	blocks := make([]any, 0, len(prefixes)+len(existing)+1)
	for _, prefix := range prefixes {
		if prefix.Text == "" {
			continue
		}
		existing = removeExactSystemBlock(existing, prefix.Text)
		blocks = append(blocks, encodedSystemBlock(prefix.Text, prefix.CacheControl))
	}
	if instructionSelected && instruction != nil && instruction.Text != "" {
		existing = removeExactSystemBlock(existing, instruction.Text)
		blocks = append(blocks, encodedSystemBlock(instruction.Text, instruction.CacheControl))
	}
	blocks = append(blocks, existing...)
	document["system"] = blocks
}

func extractSystemInstruction(document map[string]any, instruction string) bool {
	if instruction == "" {
		return false
	}
	blocks := normalizedSystemBlocks(document["system"])
	for index, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			continue
		}
		remaining, found := removeInstructionText(text, instruction)
		if !found {
			continue
		}
		if remaining == "" {
			blocks = append(blocks[:index], blocks[index+1:]...)
		} else {
			block["text"] = remaining
			delete(block, "cache_control")
		}
		document["system"] = blocks
		return true
	}
	return false
}

func removeInstructionText(text, instruction string) (string, bool) {
	before, after, found := strings.Cut(text, instruction)
	if !found {
		return text, false
	}
	before = strings.TrimSuffix(strings.TrimSuffix(before, "\r\n\r\n"), "\n\n")
	after = strings.TrimPrefix(strings.TrimPrefix(after, "\r\n\r\n"), "\n\n")
	if before == "" {
		return after, true
	}
	if after == "" {
		return before, true
	}
	return before + "\n\n" + after, true
}

func normalizedSystemBlocks(system any) []any {
	switch value := system.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": value}}
	case []any:
		return append([]any(nil), value...)
	default:
		return nil
	}
}

func removeExactSystemBlock(blocks []any, text string) []any {
	result := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if ok && block["type"] == "text" && block["text"] == text {
			continue
		}
		result = append(result, raw)
	}
	return result
}

func encodedSystemBlock(text string, cache *manifest.AnthropicCacheControl) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if cache == nil {
		return block
	}
	value := map[string]any{"type": cache.Type}
	if cache.TTL != "" {
		value["ttl"] = cache.TTL
	}
	if cache.Scope != "" {
		value["scope"] = cache.Scope
	}
	block["cache_control"] = value
	return block
}

// systemContains detects an existing manifest prefix in either supported
// system representation. It is the idempotency guard for prependSystem and
// must remain aligned with mapSystemText's representation handling.

// billing derives the exact manifest-declared attribution string from bounded
// request text and resolved client version. Its byte selection, salt, hash
// prefix, and format are plugin contract values; substituting host defaults
// changes the provider-visible client identity.
func billing(document map[string]any, policy manifest.BillingAttribution, version string) string {
	text := firstUserText(document)
	selected := make([]byte, len(policy.ByteOffsets))
	missing := policy.MissingByte[0]
	for index, offset := range policy.ByteOffsets {
		selected[index] = missing
		if offset >= 0 && offset < len(text) {
			selected[index] = text[offset]
		}
	}
	digest := sha256.Sum256([]byte(policy.Salt + string(selected) + version))
	fingerprint := hex.EncodeToString(digest[:])
	if policy.HashPrefix < len(fingerprint) {
		fingerprint = fingerprint[:policy.HashPrefix]
	}
	return strings.NewReplacer("{version}", version, "{fingerprint}", fingerprint).Replace(policy.Format)
}

// firstUserText extracts text without serializing images or tool payloads. It
// feeds bounded attribution only, so expanding it to arbitrary request content
// could leak data into a fingerprint or destabilize the declared algorithm.
func firstUserText(document map[string]any) string {
	for _, raw := range array(document["messages"]) {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		if text, ok := message["content"].(string); ok {
			return text
		}
		var parts []string
		for _, rawBlock := range array(message["content"]) {
			if block, ok := rawBlock.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// ResolveIdentity resolves the provider-visible client version and User-Agent
// exclusively from trusted manifest policy. The precedence is environment,
// bounded network probe, cache, then validated fallback. Do not hard-code a
// generic user agent, remove the fallback chain, or log private manifest values;
// downstream protected headers depend on the exact resolved strings.
func ResolveIdentity(identity *manifest.ResolvedClientIdentity) (string, string, error) {
	return clientidentity.Resolve(identity)
}

// fetchVersion performs the optional bounded HTTPS version probe declared by
// the manifest. Redirects, non-OK responses, oversized bodies, and values that
// fail the declared pattern are ignored so remote content cannot redefine the
// client identity outside policy.

// cachePath locates host-managed client-version state without consulting a
// plugin-controlled path. Keeping this under the Crux data root prevents a
// manifest from redirecting identity persistence to arbitrary files.

// cachedVersion returns a cached client version only when it still satisfies
// the current manifest pattern. A stale or cross-plugin value must not become a
// provider-visible identity merely because the cache key exists.

// persistVersion records the validated resolved version with private file
// permissions and atomic replacement. Cache failure is intentionally
// non-fatal because the manifest fallback still preserves a valid identity;
// never persist credentials or other private manifest fields here.

// readCache treats missing or malformed cache data as empty state. The cache is
// an optimization, not authority, so it must never override manifest validation
// or prevent provider construction when unavailable.

// applyHeader executes one ordered manifest header rule. This is where the
// resolved client User-Agent and version context become actual request headers.
// Preserve set, delete, and append-unique semantics exactly; bypassing this
// helper can remove identity handling without breaking basic connectivity.
func applyHeader(headers http.Header, rule manifest.HeaderRule, values map[string]string) error {
	if rule.Operation == "delete" {
		headers.Del(rule.Name)
		return nil
	}
	if rule.Value == nil {
		return fmt.Errorf("header %q has no value", rule.Name)
	}
	value, err := template(*rule.Value, values)
	if err != nil {
		return err
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
		current := splitHeader(headers.Get(rule.Name))
		if !containsFold(current, value) {
			current = append(current, value)
			headers.Set(rule.Name, strings.Join(current, ","))
		}
	default:
		return fmt.Errorf("unsupported header operation %q", rule.Operation)
	}
	return nil
}

// template evaluates the bounded header template language against values
// resolved by NewClient and RoundTrip. Do not add ambient environment, file, or
// command expansion here; plugin manifests are declarative policy, not code.
func template(value manifest.Template, contexts map[string]string) (string, error) {
	switch value.Kind {
	case "literal":
		text, ok := value.Value.(string)
		if !ok {
			return "", fmt.Errorf("header literal is not a string")
		}
		return text, nil
	case "context":
		return contexts[value.Ref], nil
	case "concat":
		var out strings.Builder
		for _, part := range value.Parts {
			text, err := template(part, contexts)
			if err != nil {
				return "", err
			}
			out.WriteString(text)
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("template kind %q unavailable in header context", value.Kind)
	}
}

type remapReader struct {
	reader   io.ReadCloser
	codec    *manifest.ToolCodec
	raw, out []byte
	err      error
	holdback int
}

// Read remaps provider tool names back to host names while retaining a bounded
// suffix so JSON tokens split across stream chunks are not corrupted. Removing
// the holdback or remapping chunks independently causes intermittent tool-call
// failures that transport health checks cannot detect.
func (r *remapReader) Read(target []byte) (int, error) {
	for {
		if len(r.out) > 0 {
			n := copy(target, r.out)
			r.out = r.out[n:]
			return n, nil
		}
		if r.err != nil && len(r.raw) == 0 {
			return 0, r.err
		}
		buffer := make([]byte, 32*1024)
		n, err := r.reader.Read(buffer)
		if n > 0 {
			r.raw = append(r.raw, buffer[:n]...)
		}
		if err != nil {
			r.err = err
		}
		if r.err != nil {
			r.out = []byte(remapText(string(r.raw), r.codec))
			r.raw = nil
		} else if len(r.raw) > r.holdback {
			safe := len(r.raw) - r.holdback
			for safe > 0 && !strings.ContainsRune("{},[]\n", rune(r.raw[safe-1])) {
				safe--
			}
			if safe == 0 {
				continue
			}
			r.out = []byte(remapText(string(r.raw[:safe]), r.codec))
			r.raw = r.raw[safe:]
		}
	}
}

// Close releases the original response body; wrappers must not swallow this
// lifecycle call or streamed provider connections will leak.
func (r *remapReader) Close() error { return r.reader.Close() }

// remapText reverses only manifest-declared tool and parameter aliases in
// streamed event text. Case folding follows manifest policy so provider naming
// differences do not leak into host dispatch.
func remapText(text string, codec *manifest.ToolCodec) string {
	pattern := regexp.MustCompile(`"name"\s*:\s*"([A-Za-z][A-Za-z0-9_-]*)"`)
	text = pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		if host, ok := inboundToolName(parts[1], codec); ok {
			return `"name":"` + host + `"`
		}
		return match
	})
	for _, mapping := range codec.Parameters {
		text = strings.ReplaceAll(text, `"`+mapping.Provider+`":`, `"`+mapping.Host+`":`)
	}
	return text
}

// deletePointer implements the intentionally bounded top-level deletion used
// by validated request transforms. Expanding JSON-pointer behavior here without
// matching validation could let a manifest remove nested user content.
func deletePointer(document map[string]any, path string) {
	if strings.Count(path, "/") == 1 {
		delete(document, strings.TrimPrefix(path, "/"))
	}
}

// readBounded enforces manifest-declared limits before buffering probe or
// request bodies. Do not replace it with an unbounded read in the plugin path;
// declarative transforms must retain predictable memory use.
func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("request exceeds %d bytes", maximum)
	}
	return data, nil
}

// array safely projects optional JSON arrays without inventing values for
// malformed or absent fields.
func array(value any) []any { result, _ := value.([]any); return result }

// hasSurface gates every tool-codec transform by the exact surfaces declared
// in the manifest. Applying aliases outside those surfaces corrupts definitions,
// history, or stream events that the plugin did not authorize changing.
func hasSurface(values []string, sought string) bool {
	for _, value := range values {
		if value == sought {
			return true
		}
	}
	return false
}

// containsFold provides HTTP-compatible case-insensitive comparison for
// protected header values and policy lists.
func containsFold(values []string, sought string) bool {
	for _, value := range values {
		if strings.EqualFold(value, sought) {
			return true
		}
	}
	return false
}

// hasPrefixFold applies manifest header-prefix deletion without depending on
// caller capitalization. A case-sensitive check can leave forbidden ambient
// identity or credential headers on the request.
func hasPrefixFold(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

// splitHeader normalizes comma-separated values for append-unique processing.
// It preserves caller features while preventing duplicate manifest values.
func splitHeader(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

// newUUID creates the process-scoped session identity injected by RoundTrip.
// It is shared per manifest endpoint through NewClient and must not be replaced
// with a constant or regenerated per request, either of which changes the
// provider-visible session contract.
func newUUID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:16])
}
