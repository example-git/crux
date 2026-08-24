// Package anthropic provides finite host-interpreted Anthropic Messages wire
// policy. All provider identity, header, prompt, metadata, and tool alias values
// come from a trusted manifest operation.
package anthropic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

var (
	processValues sync.Map
	cacheMu       sync.Mutex
)

type Client struct {
	operation *providertransport.Operation
	version   string
	userAgent string
	sessionID string
	base      http.RoundTripper
}

func NewClient(operation *providertransport.Operation, debug bool) (*http.Client, error) {
	if operation == nil || operation.Anthropic == nil {
		return nil, fmt.Errorf("Anthropic operation has no wire policy")
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
	return &http.Client{Transport: &Client{operation: operation.Clone(), version: version, userAgent: userAgent, sessionID: session.(string), base: base}}, nil
}

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
		request.ContentLength = int64(len(rewritten))
	}
	response, err := c.base.RoundTrip(request)
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
	for _, prefix := range policy.SystemLinePrefixes {
		filterSystemLines(document, prefix)
	}
	if operation.ToolCodec != nil {
		remapTools(document, operation.ToolCodec)
		remapReferences(document, operation.ToolCodec)
	}
	prefixes := append([]string(nil), policy.SystemPrefixes...)
	if policy.Billing != nil {
		prefixes = append([]string{billing(document, *policy.Billing, version)}, prefixes...)
	}
	if len(prefixes) > 0 {
		prependSystem(document, prefixes)
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

func remapTools(document map[string]any, codec *manifest.ToolCodec) {
	outbound := map[string]string{}
	params := map[string]map[string]string{}
	for _, alias := range codec.Aliases {
		outbound[alias.Host] = alias.Provider
	}
	for _, mapping := range codec.Parameters {
		if params[mapping.Tool] == nil {
			params[mapping.Tool] = map[string]string{}
		}
		params[mapping.Tool][mapping.Host] = mapping.Provider
	}
	if hasSurface(codec.Surfaces, "definitions") {
		for _, raw := range array(document["tools"]) {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			host, _ := tool["name"].(string)
			provider, ok := outbound[host]
			if !ok {
				continue
			}
			tool["name"] = provider
			remapSchema(tool, params[host])
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
			name, _ := block["name"].(string)
			provider, found := outbound[name]
			if !found {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if hasSurface(codec.Surfaces, "history-calls") {
					block["name"] = provider
					remapInput(block, params[name])
				}
			case "tool_result":
				if hasSurface(codec.Surfaces, "history-results") {
					block["name"] = provider
				}
			}
		}
	}
}

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

func remapInput(block map[string]any, renames map[string]string) {
	input, _ := block["input"].(map[string]any)
	for from, to := range renames {
		if value, ok := input[from]; ok {
			input[to] = value
			delete(input, from)
		}
	}
}

func remapReferences(document map[string]any, codec *manifest.ToolCodec) {
	if !hasSurface(codec.Surfaces, "prompt-references") {
		return
	}
	pairs := make([]string, 0, len(codec.Aliases)*2)
	for _, alias := range codec.Aliases {
		pairs = append(pairs, "`"+alias.Host+"`", "`"+alias.Provider+"`")
	}
	replacer := strings.NewReplacer(pairs...)
	mapSystemText(document, func(value string) string { return replacer.Replace(value) })
	for _, raw := range array(document["tools"]) {
		if tool, ok := raw.(map[string]any); ok {
			if text, ok := tool["description"].(string); ok {
				tool["description"] = replacer.Replace(text)
			}
		}
	}
}

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

func prependSystem(document map[string]any, prefixes []string) {
	blocks := make([]any, 0, len(prefixes)+4)
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if systemContains(document["system"], prefix) {
			return
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": prefix})
	}
	switch system := document["system"].(type) {
	case string:
		blocks = append(blocks, map[string]any{"type": "text", "text": system})
	case []any:
		blocks = append(blocks, system...)
	}
	document["system"] = blocks
}

func systemContains(system any, sought string) bool {
	switch typed := system.(type) {
	case string:
		return strings.Contains(typed, sought)
	case []any:
		for _, raw := range typed {
			if block, ok := raw.(map[string]any); ok {
				if text, ok := block["text"].(string); ok && strings.Contains(text, sought) {
					return true
				}
			}
		}
	}
	return false
}

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

// ResolveIdentity resolves the manifest-declared version and user agent.
func ResolveIdentity(identity *manifest.ResolvedClientIdentity) (string, string, error) {
	if identity == nil {
		return "", "", nil
	}
	pattern, err := regexp.Compile(identity.VersionPattern)
	if err != nil {
		return "", "", err
	}
	version := ""
	if candidate := os.Getenv(identity.Environment); pattern.MatchString(candidate) {
		version = candidate
	}
	if version == "" {
		version = fetchVersion(identity, pattern)
	}
	if version == "" {
		version = cachedVersion(identity.CacheKey, pattern)
	}
	if version == "" {
		version = identity.FallbackVersion
	}
	if !pattern.MatchString(version) {
		return "", "", fmt.Errorf("resolved client version is invalid")
	}
	persistVersion(identity.CacheKey, version)
	return version, strings.ReplaceAll(identity.UserAgentFormat, "{version}", version), nil
}

func fetchVersion(identity *manifest.ResolvedClientIdentity, pattern *regexp.Regexp) string {
	if identity.LatestURL == "" {
		return ""
	}
	client := &http.Client{Timeout: time.Duration(identity.ProbeTimeoutMS) * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, identity.LatestURL, nil)
	if err != nil {
		return ""
	}
	response, err := client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	data, err := readBounded(response.Body, identity.ProbeMaxBytes)
	if err != nil {
		return ""
	}
	candidate := strings.TrimSpace(string(data))
	if pattern.MatchString(candidate) {
		return candidate
	}
	return ""
}

func cachePath() string {
	if directory := strings.TrimSpace(os.Getenv("AI_CLI_DIR")); directory != "" {
		return filepath.Join(directory, "provider-client-versions.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ai-cli", "provider-client-versions.json")
}

func cachedVersion(key string, pattern *regexp.Regexp) string {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	values := readCache()
	if pattern.MatchString(values[key]) {
		return values[key]
	}
	return ""
}

func persistVersion(key, value string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	values := readCache()
	values[key] = value
	path := cachePath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	data, _ := json.Marshal(values)
	temporary := path + ".tmp"
	if os.WriteFile(temporary, data, 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func readCache() map[string]string {
	result := map[string]string{}
	path := cachePath()
	if path == "" {
		return result
	}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &result)
	}
	return result
}

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
func (r *remapReader) Close() error { return r.reader.Close() }
func remapText(text string, codec *manifest.ToolCodec) string {
	reverse := map[string]string{}
	for _, alias := range codec.Aliases {
		key := alias.Provider
		if codec.CaseFoldInbound {
			key = strings.ToLower(key)
		}
		reverse[key] = alias.Host
	}
	pattern := regexp.MustCompile(`"name"\s*:\s*"([A-Za-z][A-Za-z0-9_]*)"`)
	text = pattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		key := parts[1]
		if codec.CaseFoldInbound {
			key = strings.ToLower(key)
		}
		if host, ok := reverse[key]; ok {
			return `"name":"` + host + `"`
		}
		return match
	})
	for _, mapping := range codec.Parameters {
		text = strings.ReplaceAll(text, `"`+mapping.Provider+`":`, `"`+mapping.Host+`":`)
	}
	return text
}

func deletePointer(document map[string]any, path string) {
	if strings.Count(path, "/") == 1 {
		delete(document, strings.TrimPrefix(path, "/"))
	}
}

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
func array(value any) []any { result, _ := value.([]any); return result }
func hasSurface(values []string, sought string) bool {
	for _, value := range values {
		if value == sought {
			return true
		}
	}
	return false
}

func containsFold(values []string, sought string) bool {
	for _, value := range values {
		if strings.EqualFold(value, sought) {
			return true
		}
	}
	return false
}

func hasPrefixFold(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func splitHeader(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func newUUID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:16])
}
