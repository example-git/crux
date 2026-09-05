package providertransport

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type TemplateValues struct {
	Config      map[string]any
	Credentials map[string]string
	Context     map[string]string
}

func EvaluateTemplate(value manifest.Template, values TemplateValues) (any, error) {
	switch value.Kind {
	case "literal":
		return value.Value, nil
	case "config":
		result, ok := values.Config[value.Ref]
		if !ok {
			return nil, fmt.Errorf("configuration value %q is unavailable", value.Ref)
		}
		return result, nil
	case "credential":
		result, ok := values.Credentials[value.Ref]
		if !ok || result == "" {
			return nil, fmt.Errorf("credential %q is unavailable", value.Ref)
		}
		return result, nil
	case "context":
		result, ok := values.Context[value.Ref]
		if !ok {
			return nil, fmt.Errorf("context value %q is unavailable", value.Ref)
		}
		return result, nil
	case "concat":
		var result strings.Builder
		for _, part := range value.Parts {
			item, err := EvaluateTemplate(part, values)
			if err != nil {
				return nil, err
			}
			result.WriteString(stringValue(item))
		}
		return result.String(), nil
	case "uuid":
		return uuid.NewString(), nil
	case "unix-time":
		return time.Now().Unix(), nil
	case "random-hex":
		data := make([]byte, value.Bytes)
		if _, err := rand.Read(data); err != nil {
			return nil, err
		}
		return hex.EncodeToString(data), nil
	default:
		return nil, fmt.Errorf("unsupported template kind %q", value.Kind)
	}
}

func (o *Operation) BindTemplates(values TemplateValues) (*Operation, error) {
	result := o.Clone()
	if result == nil {
		return nil, nil
	}
	for i := range result.Headers {
		template := result.Headers[i].Value
		if template == nil || template.Kind == "context" {
			continue
		}
		value, err := EvaluateTemplate(*template, values)
		if err != nil {
			return nil, fmt.Errorf("operation %q header %q: %w", result.ID, result.Headers[i].Name, err)
		}
		result.Headers[i].Value = &manifest.Template{Kind: "literal", Value: value}
	}
	return result, nil
}

func (o *Operation) ApplyHeadersWithValues(configured map[string]string, values TemplateValues) (map[string]string, error) {
	if o == nil {
		return configured, nil
	}
	result := make(map[string]string, len(configured))
	for name, value := range configured {
		result[name] = value
	}
	for _, rule := range o.Headers {
		name := rule.Name
		if rule.Operation == "delete" {
			deleteHeader(result, name)
			continue
		}
		if rule.Value == nil {
			return nil, fmt.Errorf("operation %q header %q has no value", o.ID, name)
		}
		raw, err := EvaluateTemplate(*rule.Value, values)
		if err != nil {
			return nil, fmt.Errorf("operation %q header %q: %w", o.ID, name, err)
		}
		value := stringValue(raw)
		currentName, current := headerValue(result, name)
		switch rule.Operation {
		case "set":
			if currentName != "" && currentName != name {
				delete(result, currentName)
			}
			result[name] = value
		case "set-if-absent":
			if currentName == "" {
				result[name] = value
			}
		case "append":
			if currentName == "" {
				result[name] = value
			} else {
				result[currentName] = current + "," + value
			}
		case "append-unique":
			found := false
			for _, item := range strings.Split(current, ",") {
				if strings.TrimSpace(item) == value {
					found = true
				}
			}
			if !found {
				if currentName == "" {
					result[name] = value
				} else if current == "" {
					result[currentName] = value
				} else {
					result[currentName] = current + "," + value
				}
			}
		default:
			return nil, fmt.Errorf("operation %q uses unsupported header operation %q", o.ID, rule.Operation)
		}
	}
	return result, nil
}

func ApplyJSONPipeline(document any, pipeline *manifest.JSONPipeline, values TemplateValues) error {
	if pipeline == nil {
		return nil
	}
	if pipeline.MaxOperations > 0 && len(pipeline.Operations) > pipeline.MaxOperations {
		return fmt.Errorf("JSON pipeline exceeds its operation limit")
	}
	for _, operation := range pipeline.Operations {
		if err := applyJSONOperation(document, operation, values); err != nil {
			return err
		}
	}
	return nil
}

func ApplyPromptPipeline(document map[string]any, pipeline *manifest.PromptPipeline, values TemplateValues) error {
	if pipeline == nil {
		return nil
	}
	messages, _ := document["messages"].([]any)
	for _, operation := range pipeline.Operations {
		if operation.When != nil {
			matched, err := EvaluatePredicate(document, *operation.When, values)
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
		}
		text := ""
		if operation.Text != nil {
			value, err := EvaluateTemplate(*operation.Text, values)
			if err != nil {
				return err
			}
			text = stringValue(value)
		}
		switch operation.Operation {
		case "prepend":
			messages = append([]any{promptMessage(operation.Role, text)}, messages...)
		case "append":
			messages = append(messages, promptMessage(operation.Role, text))
		case "insert-after-role":
			index := -1
			for i, raw := range messages {
				message, _ := raw.(map[string]any)
				if message["role"] == operation.Role {
					index = i
				}
			}
			if index < 0 {
				return fmt.Errorf("prompt transform role %q was not found", operation.Role)
			}
			messages = append(messages[:index+1], append([]any{promptMessage(operation.Role, text)}, messages[index+1:]...)...)
		case "drop-role":
			filtered := messages[:0]
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if message["role"] != operation.Role {
					filtered = append(filtered, raw)
				}
			}
			messages = filtered
		case "remove-lines-with-prefix":
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if operation.Role != "" && message["role"] != operation.Role {
					continue
				}
				message["content"] = filterLines(message["content"], operation.Prefix)
			}
		case "join-adjacent-role":
			joined := make([]any, 0, len(messages))
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if len(joined) > 0 && message["role"] == operation.Role {
					previous, _ := joined[len(joined)-1].(map[string]any)
					if previous["role"] == operation.Role {
						previous["content"] = stringValue(previous["content"]) + "\n" + stringValue(message["content"])
						continue
					}
				}
				joined = append(joined, raw)
			}
			messages = joined
		default:
			return fmt.Errorf("unsupported prompt operation %q", operation.Operation)
		}
	}
	document["messages"] = messages
	return nil
}

func ApplyRoleMap(document map[string]any, mapping *manifest.RoleMap) error {
	if mapping == nil {
		return nil
	}
	messages, _ := document["messages"].([]any)
	roles := map[string]string{"system": mapping.System, "developer": mapping.Developer, "user": mapping.User, "assistant": mapping.Assistant, "tool": mapping.Tool}
	filtered := messages[:0]
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("prompt message is not an object")
		}
		role, _ := message["role"].(string)
		mapped, known := roles[role]
		if !known || mapped == "" {
			switch mapping.Unknown {
			case "drop", "warn-drop":
				continue
			default:
				return fmt.Errorf("role %q has no provider mapping", role)
			}
		}
		message["role"] = mapped
		filtered = append(filtered, message)
	}
	document["messages"] = filtered
	return nil
}

func SetJSONPointer(document any, path string, value any, onlyAbsent bool) error {
	segments, err := pointerSegments(path)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return fmt.Errorf("root replacement is unsupported")
	}
	current := document
	for _, segment := range segments[:len(segments)-1] {
		object, ok := current.(map[string]any)
		if !ok {
			return fmt.Errorf("JSON pointer %q traverses a non-object", path)
		}
		next, ok := object[segment]
		if !ok {
			next = map[string]any{}
			object[segment] = next
		}
		current = next
	}
	object, ok := current.(map[string]any)
	if !ok {
		return fmt.Errorf("JSON pointer %q targets a non-object", path)
	}
	key := segments[len(segments)-1]
	if onlyAbsent {
		if _, exists := object[key]; exists {
			return nil
		}
	}
	object[key] = value
	return nil
}

func JSONPointer(document any, path string) any {
	value, ok, err := LookupJSONPointer(document, path)
	if err != nil || !ok {
		return nil
	}
	return value
}

func LookupJSONPointer(document any, path string) (any, bool, error) {
	segments, err := pointerSegments(path)
	if err != nil {
		return nil, false, err
	}
	current := document
	for _, segment := range segments {
		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[segment]
			if !ok {
				return nil, false, nil
			}
		case []any:
			if segment == "" || (len(segment) > 1 && segment[0] == '0') {
				return nil, false, fmt.Errorf("JSON pointer %q uses an invalid array index", path)
			}
			for _, character := range segment {
				if character < '0' || character > '9' {
					return nil, false, fmt.Errorf("JSON pointer %q uses an invalid array index", path)
				}
			}
			index, err := strconv.ParseUint(segment, 10, 64)
			if err != nil {
				return nil, false, fmt.Errorf("JSON pointer %q uses an invalid array index", path)
			}
			if index >= uint64(len(value)) {
				return nil, false, nil
			}
			current = value[index]
		default:
			return nil, false, fmt.Errorf("JSON pointer %q traverses a non-container", path)
		}
	}
	return current, true, nil
}

func applyJSONOperation(document any, operation manifest.JSONOperation, values TemplateValues) error {
	var value any
	var err error
	if operation.Value != nil {
		value, err = EvaluateTemplate(*operation.Value, values)
		if err != nil {
			return err
		}
	}
	switch operation.Operation {
	case "set":
		return SetJSONPointer(document, operation.Path, value, false)
	case "set-if-absent":
		return SetJSONPointer(document, operation.Path, value, true)
	case "delete":
		return deleteJSONPointer(document, operation.Path)
	case "copy", "move":
		value = JSONPointer(document, operation.From)
		if value == nil {
			return fmt.Errorf("JSON pointer %q is unavailable", operation.From)
		}
		if err := SetJSONPointer(document, operation.Path, value, false); err != nil {
			return err
		}
		if operation.Operation == "move" {
			return deleteJSONPointer(document, operation.From)
		}
		return nil
	case "rename-key":
		value = JSONPointer(document, operation.From)
		if value == nil {
			return fmt.Errorf("JSON pointer %q is unavailable", operation.From)
		}
		if err := SetJSONPointer(document, operation.Path, value, false); err != nil {
			return err
		}
		return deleteJSONPointer(document, operation.From)
	case "keep-keys", "drop-keys":
		object, ok := JSONPointer(document, operation.Path).(map[string]any)
		if !ok {
			return fmt.Errorf("JSON pointer %q is not an object", operation.Path)
		}
		allowed := make(map[string]bool, len(operation.Keys))
		for _, key := range operation.Keys {
			allowed[key] = true
		}
		for key := range object {
			if operation.Operation == "keep-keys" && !allowed[key] || operation.Operation == "drop-keys" && allowed[key] {
				delete(object, key)
			}
		}
		return nil
	case "filter-array":
		array, ok := JSONPointer(document, operation.Path).([]any)
		if !ok || operation.Predicate == nil {
			return fmt.Errorf("JSON pointer %q is not a filterable array", operation.Path)
		}
		filtered := array[:0]
		for _, item := range array {
			matched, err := EvaluatePredicate(item, *operation.Predicate, values)
			if err != nil {
				return err
			}
			if matched {
				filtered = append(filtered, item)
			}
		}
		return SetJSONPointer(document, operation.Path, filtered, false)
	default:
		return fmt.Errorf("unsupported JSON operation %q", operation.Operation)
	}
}

func EvaluatePredicate(document any, predicate manifest.Predicate, values TemplateValues) (bool, error) {
	actual := JSONPointer(document, predicate.Path)
	var expected any
	var err error
	if predicate.Value != nil {
		expected, err = EvaluateTemplate(*predicate.Value, values)
		if err != nil {
			return false, err
		}
	}
	switch predicate.Operation {
	case "exists":
		return actual != nil, nil
	case "equals":
		return jsonEqual(actual, expected), nil
	case "not-equals":
		return !jsonEqual(actual, expected), nil
	case "contains":
		return strings.Contains(stringValue(actual), stringValue(expected)), nil
	case "starts-with":
		return strings.HasPrefix(stringValue(actual), stringValue(expected)), nil
	case "matches-enum":
		for _, candidate := range predicate.Values {
			value, err := EvaluateTemplate(candidate, values)
			if err != nil {
				return false, err
			}
			if jsonEqual(actual, value) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unsupported predicate %q", predicate.Operation)
	}
}

func deleteJSONPointer(document any, path string) error {
	segments, err := pointerSegments(path)
	if err != nil || len(segments) == 0 {
		return fmt.Errorf("invalid delete path %q", path)
	}
	current := document
	for _, segment := range segments[:len(segments)-1] {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[segment]
	}
	if object, ok := current.(map[string]any); ok {
		delete(object, segments[len(segments)-1])
	}
	return nil
}

func pointerSegments(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q", path)
	}
	raw := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range raw {
		raw[i] = strings.ReplaceAll(strings.ReplaceAll(raw[i], "~1", "/"), "~0", "~")
	}
	return raw, nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func JSONValuesEqual(left, right any) bool {
	leftText, leftNumber := jsonNumberText(left)
	rightText, rightNumber := jsonNumberText(right)
	if leftNumber || rightNumber {
		if !leftNumber || !rightNumber {
			return false
		}
		leftValue, leftOK := normalizeJSONNumber(leftText)
		rightValue, rightOK := normalizeJSONNumber(rightText)
		return leftOK && rightOK &&
			leftValue.negative == rightValue.negative &&
			leftValue.digits == rightValue.digits &&
			leftValue.exponent.Cmp(rightValue.exponent) == 0
	}
	a, aErr := json.Marshal(left)
	b, bErr := json.Marshal(right)
	return aErr == nil && bErr == nil && string(a) == string(b)
}

func jsonEqual(left, right any) bool {
	return JSONValuesEqual(left, right)
}

func jsonNumberText(value any) (string, bool) {
	switch value := value.(type) {
	case int:
		return strconv.FormatInt(int64(value), 10), true
	case int8:
		return strconv.FormatInt(int64(value), 10), true
	case int16:
		return strconv.FormatInt(int64(value), 10), true
	case int32:
		return strconv.FormatInt(int64(value), 10), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case uint:
		return strconv.FormatUint(uint64(value), 10), true
	case uint8:
		return strconv.FormatUint(uint64(value), 10), true
	case uint16:
		return strconv.FormatUint(uint64(value), 10), true
	case uint32:
		return strconv.FormatUint(uint64(value), 10), true
	case uint64:
		return strconv.FormatUint(value, 10), true
	case float32:
		return strconv.FormatFloat(float64(value), 'g', -1, 32), true
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), true
	case json.Number:
		return value.String(), true
	default:
		return "", false
	}
}

type normalizedJSONNumber struct {
	negative bool
	digits   string
	exponent *big.Int
}

func normalizeJSONNumber(text string) (*normalizedJSONNumber, bool) {
	if text == "" || text != strings.TrimSpace(text) || !json.Valid([]byte(text)) {
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

func promptMessage(role, text string) map[string]any {
	if role == "" {
		role = "system"
	}
	return map[string]any{"role": role, "content": text}
}

func filterLines(value any, prefix string) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	lines := strings.Split(text, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			filtered = append(filtered, line)
		}
	}
	return strings.Join(filtered, "\n")
}

func headerValue(headers map[string]string, name string) (string, string) {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return key, value
		}
	}
	return "", ""
}

func deleteHeader(headers map[string]string, name string) {
	if key, _ := headerValue(headers, name); key != "" {
		delete(headers, key)
	}
}
