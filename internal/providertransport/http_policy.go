package providertransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

type PolicyTransport struct {
	Base            http.RoundTripper
	Operation       *Operation
	Values          TemplateValues
	Headers         map[string]string
	RuntimeControls map[string]any
}

func (t *PolicyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.Operation == nil {
		return nil, fmt.Errorf("provider operation policy is unavailable")
	}
	clone := request.Clone(request.Context())
	base, err := url.Parse(t.Operation.Endpoint.BaseURL)
	if err != nil {
		return nil, err
	}
	reference, err := url.Parse(t.Operation.Path)
	if err != nil {
		return nil, err
	}
	clone.URL = base.ResolveReference(reference)
	if t.Operation.Method != "" {
		clone.Method = t.Operation.Method
	}
	for name, value := range t.Headers {
		clone.Header.Set(name, value)
	}
	if clone.Body != nil && clone.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(clone.Body, 32<<20))
		_ = clone.Body.Close()
		if err != nil {
			return nil, err
		}
		rewritten, err := t.rewriteRequest(clone.Context(), body)
		if err != nil {
			return nil, err
		}
		clone.Body = io.NopCloser(bytes.NewReader(rewritten))
		clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
		clone.ContentLength = int64(len(rewritten))
	}
	baseTransport := t.Base
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	response, err := RoundTripWithRetry(clone, baseTransport, t.Operation.Retry, t.Operation.Errors)
	if err != nil || response == nil || response.Body == nil || t.Operation.ResponseTransform == nil {
		return response, err
	}
	contentType := response.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		response.Body = &transformSSEBody{source: response.Body, transform: t.rewriteResponse}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		return response, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	rewritten, err := t.rewriteResponse(body)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(rewritten))
	response.ContentLength = int64(len(rewritten))
	response.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))
	return response, nil
}

func (t *PolicyTransport) rewriteRequest(ctx context.Context, body []byte) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("operation %q request is not valid JSON: %w", t.Operation.ID, err)
	}
	messages, restore := normalizePrompt(document)
	if messages != nil {
		document["messages"] = messages
		if err := ApplyPromptPipeline(document, t.Operation.PromptTransform, t.Values); err != nil {
			return nil, err
		}
		if err := ApplyRoleMap(document, t.Operation.RoleMap); err != nil {
			return nil, err
		}
		restore(document)
	}
	for path, value := range t.RuntimeControls {
		if err := SetJSONPointer(document, path, value, true); err != nil {
			return nil, err
		}
	}
	if err := ApplyJSONPipeline(document, t.Operation.RequestTransform, t.Values); err != nil {
		return nil, err
	}
	for path, value := range fantasy.RuntimeControlsFromContext(ctx) {
		if err := SetJSONPointer(document, path, value, false); err != nil {
			return nil, err
		}
	}
	return json.Marshal(document)
}

func (t *PolicyTransport) rewriteResponse(body []byte) ([]byte, error) {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	if err := ApplyJSONPipeline(document, t.Operation.ResponseTransform, t.Values); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func normalizePrompt(document map[string]any) ([]any, func(map[string]any)) {
	if messages, ok := document["messages"].([]any); ok {
		return messages, func(map[string]any) {}
	}
	input, ok := document["input"].([]any)
	if !ok {
		return nil, func(map[string]any) {}
	}
	marker := "__crux_responses_input_index"
	for markerExists(input, marker) {
		marker += "_"
	}
	messages := make([]any, 0, len(input))
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["role"].(string); ok {
			item[marker] = index
			messages = append(messages, item)
		}
	}
	return messages, func(value map[string]any) {
		transformed, _ := value["messages"].([]any)
		delete(value, "messages")
		byIndex := make(map[int]int, len(transformed))
		for index, raw := range transformed {
			message, _ := raw.(map[string]any)
			if originalIndex, ok := promptItemIndex(message[marker]); ok {
				byIndex[originalIndex] = index
			}
		}
		result := make([]any, 0, len(input)+len(transformed))
		transformedIndex := 0
		for originalIndex, raw := range input {
			messageIndex, roleBearing := byIndex[originalIndex]
			if roleBearing {
				for transformedIndex <= messageIndex {
					message, _ := transformed[transformedIndex].(map[string]any)
					delete(message, marker)
					result = append(result, message)
					transformedIndex++
				}
				continue
			}
			item, _ := raw.(map[string]any)
			if _, wasRoleBearing := item[marker]; wasRoleBearing {
				delete(item, marker)
				continue
			}
			result = append(result, raw)
		}
		for transformedIndex < len(transformed) {
			message, _ := transformed[transformedIndex].(map[string]any)
			delete(message, marker)
			result = append(result, message)
			transformedIndex++
		}
		value["input"] = result
	}
}

func markerExists(input []any, marker string) bool {
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if _, exists := item[marker]; exists {
			return true
		}
	}
	return false
}

func promptItemIndex(value any) (int, bool) {
	switch index := value.(type) {
	case int:
		return index, index >= 0
	case float64:
		if index >= 0 && index == float64(int(index)) {
			return int(index), true
		}
	}
	return 0, false
}

type transformSSEBody struct {
	source    io.ReadCloser
	transform func([]byte) ([]byte, error)
	reader    *bufio.Reader
	pending   bytes.Buffer
	err       error
}

func (r *transformSSEBody) Read(buffer []byte) (int, error) {
	if r.reader == nil {
		r.reader = bufio.NewReaderSize(r.source, 64<<10)
	}
	for r.pending.Len() == 0 && r.err == nil {
		line, err := r.reader.ReadString('\n')
		if len(line) > 16<<20 {
			r.err = fmt.Errorf("SSE line exceeds %d bytes", 16<<20)
			break
		}
		if len(line) > 0 {
			trimmed := strings.TrimSuffix(line, "\n")
			ending := line[len(trimmed):]
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					rewritten, transformErr := r.transform([]byte(payload))
					if transformErr != nil {
						r.err = transformErr
						break
					}
					trimmed = "data: " + string(rewritten)
				}
			}
			r.pending.WriteString(trimmed)
			r.pending.WriteString(ending)
		}
		if err != nil {
			r.err = err
		}
	}
	if r.pending.Len() > 0 {
		return r.pending.Read(buffer)
	}
	return 0, r.err
}

func (r *transformSSEBody) Close() error { return r.source.Close() }
