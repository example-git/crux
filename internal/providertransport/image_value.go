package providertransport

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/google/uuid"
)

const imageValueLimit = 512 << 20

var errImageValueAbsent = errors.New("image value reference is absent")

type imageOmittedValue struct{}

func (imageOmittedValue) MarshalJSON() ([]byte, error) {
	return nil, errors.New("omitted image value is not valid in this context")
}

func imageValueOmitted(value any) bool {
	_, ok := value.(imageOmittedValue)
	return ok
}

type imageEvaluation struct {
	remaining   int
	credentials map[string]bool
}

func EvaluateImageValue(value manifest.ImageValue, context map[string]any) (any, error) {
	evaluation := imageEvaluation{remaining: 100000}
	result, err := evaluation.value(value, context)
	if err != nil {
		return nil, err
	}
	return evaluation.scoped(result), nil
}

func (e *imageEvaluation) value(value manifest.ImageValue, context map[string]any) (any, error) {
	e.remaining--
	if e.remaining < 0 {
		return nil, errors.New("image expression evaluation limit exceeded")
	}
	switch {
	case value.Literal != nil:
		decoder := json.NewDecoder(bytes.NewReader(value.Literal))
		decoder.UseNumber()
		var result any
		if err := decoder.Decode(&result); err != nil {
			return nil, errors.New("invalid image literal")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("trailing image literal data")
		}
		return result, nil
	case value.Ref != "":
		return e.pointer(context, value.Ref)
	case value.Object != nil:
		result := make(map[string]any, len(value.Object))
		for name, child := range value.Object {
			resolved, err := e.value(child, context)
			if err != nil {
				return nil, err
			}
			if !imageValueOmitted(resolved) {
				result[name] = resolved
			}
		}
		return result, nil
	case value.Array != nil:
		result := make([]any, 0, len(value.Array))
		for _, child := range value.Array {
			resolved, err := e.value(child, context)
			if err != nil {
				return nil, err
			}
			if !imageValueOmitted(resolved) {
				result = append(result, resolved)
			}
		}
		return result, nil
	case value.Op != "":
		return e.operation(value, context)
	default:
		return nil, errors.New("empty image expression")
	}
}

func imagePointer(value any, pointer string) (any, error) {
	evaluation := imageEvaluation{remaining: 100000}
	return evaluation.pointer(value, pointer)
}

func (e *imageEvaluation) pointer(value any, pointer string) (any, error) {
	if pointer == "" {
		return e.materialize(value)
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("invalid image value pointer")
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch container := e.unwrap(value).(type) {
		case map[string]any:
			var ok bool
			value, ok = container[part]
			if !ok {
				return nil, errImageValueAbsent
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || strconv.Itoa(index) != part {
				return nil, errors.New("invalid image array index")
			}
			if index >= len(container) {
				return nil, errImageValueAbsent
			}
			value = container[index]
		default:
			return nil, errors.New("image reference is not a container")
		}
	}
	return e.materialize(value)
}

func imageString(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case json.Number:
		return string(typed), nil
	case int:
		return strconv.Itoa(typed), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case bool:
		return strconv.FormatBool(typed), nil
	default:
		return "", errors.New("image expression requires a scalar string")
	}
}

func imageInteger(value any) (int64, error) {
	text, err := imageString(value)
	if err != nil {
		return 0, err
	}
	result, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.New("image expression requires an integer")
	}
	return result, nil
}

func (e *imageEvaluation) operation(value manifest.ImageValue, context map[string]any) (any, error) {
	if value.Op == "if" {
		if len(value.Args) != 3 {
			return nil, errors.New("image conditional requires three arguments")
		}
		condition, err := e.value(value.Args[0], context)
		if err != nil {
			return nil, err
		}
		flag, ok := condition.(bool)
		if !ok {
			return nil, errors.New("image conditional requires a boolean")
		}
		index := 2
		if flag {
			index = 1
		}
		return e.value(value.Args[index], context)
	}
	if value.Op == "optional" {
		if len(value.Args) != 1 {
			return nil, errors.New("optional image value requires one argument")
		}
		result, err := e.value(value.Args[0], context)
		if errors.Is(err, errImageValueAbsent) {
			return nil, nil
		}
		return result, err
	}
	if value.Op == "coalesce" {
		for _, arg := range value.Args {
			result, err := e.value(arg, context)
			if err != nil {
				if errors.Is(err, errImageValueAbsent) {
					continue
				}
				return nil, err
			}
			if result != nil && result != "" {
				return result, nil
			}
		}
		return nil, errors.New("image expression has no available value")
	}
	if value.Op == "map" || value.Op == "filter" {
		if len(value.Args) != 2 {
			return nil, errors.New("image collection operation requires two arguments")
		}
		input, err := e.value(value.Args[0], context)
		if err != nil {
			return nil, err
		}
		items, ok := input.([]any)
		if !ok || len(items) > 4096 {
			return nil, errors.New("image collection is invalid or exceeds limit")
		}
		result := make([]any, 0, len(items))
		for index, item := range items {
			child := make(map[string]any, len(context)+2)
			for k, v := range context {
				child[k] = v
			}
			child["item"] = item
			child["index"] = index
			resolved, err := e.value(value.Args[1], child)
			if err != nil {
				return nil, err
			}
			if value.Op == "map" {
				result = append(result, resolved)
			} else {
				keep, ok := resolved.(bool)
				if !ok {
					return nil, errors.New("image filter requires a boolean")
				}
				if keep {
					result = append(result, item)
				}
			}
		}
		return result, nil
	}
	args := make([]any, len(value.Args))
	for index, arg := range value.Args {
		result, err := e.value(arg, context)
		if err != nil {
			return nil, err
		}
		args[index] = result
	}
	arity := func(count int) error {
		if len(args) != count {
			return errors.New("invalid image operation argument count")
		}
		return nil
	}
	text := func(index int) (string, error) {
		if index >= len(args) {
			return "", errors.New("missing image argument")
		}
		return imageString(args[index])
	}
	switch value.Op {
	case "omit":
		if err := arity(0); err != nil {
			return nil, err
		}
		return imageOmittedValue{}, nil
	case "add", "less":
		if err := arity(2); err != nil {
			return nil, err
		}
		left, err := imageInteger(args[0])
		if err != nil {
			return nil, err
		}
		right, err := imageInteger(args[1])
		if err != nil {
			return nil, err
		}
		if value.Op == "less" {
			return left < right, nil
		}
		if right > 0 && left > math.MaxInt64-right || right < 0 && left < math.MinInt64-right {
			return nil, errors.New("image integer addition overflow")
		}
		return left + right, nil
	case "uuid":
		if err := arity(0); err != nil {
			return nil, err
		}
		return uuid.NewString(), nil
	case "random":
		if err := arity(1); err != nil {
			return nil, err
		}
		maximum, err := imageInteger(args[0])
		if err != nil || maximum < 1 {
			return nil, errors.New("invalid image random bound")
		}
		number, err := rand.Int(rand.Reader, big.NewInt(maximum))
		if err != nil {
			return nil, err
		}
		return number.Int64(), nil
	case "json":
		if err := arity(1); err != nil {
			return nil, err
		}
		data, err := json.Marshal(args[0])
		if err != nil || len(data) > imageValueLimit {
			return nil, errors.New("image JSON encoding failed or exceeds limit")
		}
		return string(data), nil
	case "parse-json":
		if err := arity(1); err != nil {
			return nil, err
		}
		input, err := text(0)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(strings.NewReader(input))
		decoder.UseNumber()
		var result any
		if err := decoder.Decode(&result); err != nil {
			return nil, errors.New("invalid embedded image JSON")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("trailing embedded image JSON")
		}
		return result, nil
	case "base64", "base64-decode", "base64url-decode":
		if err := arity(1); err != nil {
			return nil, err
		}
		var data []byte
		if binary, ok := args[0].([]byte); ok {
			data = binary
		} else {
			input, err := text(0)
			if err != nil {
				return nil, err
			}
			data = []byte(input)
		}
		if len(data) > imageValueLimit*3/4 {
			return nil, errors.New("image base64 value exceeds limit")
		}
		if value.Op == "base64" {
			return base64.StdEncoding.EncodeToString(data), nil
		}
		encoding := base64.StdEncoding.Strict()
		if value.Op == "base64url-decode" {
			encoding = base64.RawURLEncoding.Strict()
		}
		decoded, err := encoding.DecodeString(string(data))
		if err != nil {
			return nil, errors.New("invalid image base64")
		}
		return string(decoded), nil
	case "data-url":
		if err := arity(2); err != nil {
			return nil, err
		}
		media, err := text(0)
		if err != nil {
			return nil, err
		}
		data, ok := args[1].([]byte)
		if !ok {
			return nil, errors.New("image data URL requires binary data")
		}
		if len(data) > imageValueLimit*3/4 {
			return nil, errors.New("image data URL exceeds limit")
		}
		return "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	case "concat":
		var result strings.Builder
		for _, arg := range args {
			part, err := imageString(arg)
			if err != nil {
				return nil, err
			}
			if len(part) > imageValueLimit-result.Len() {
				return nil, errors.New("image concatenation exceeds limit")
			}
			result.WriteString(part)
		}
		return result.String(), nil
	case "replace":
		if err := arity(3); err != nil {
			return nil, err
		}
		input, err := text(0)
		if err != nil {
			return nil, err
		}
		old, err := text(1)
		if err != nil {
			return nil, err
		}
		replacement, err := text(2)
		if err != nil {
			return nil, err
		}
		if old == "" {
			return nil, errors.New("empty image replacement pattern")
		}
		count := strings.Count(input, old)
		if len(replacement) > len(old) && count > (imageValueLimit-len(input))/(len(replacement)-len(old)) {
			return nil, errors.New("image replacement exceeds limit")
		}
		return strings.ReplaceAll(input, old, replacement), nil
	case "regexp":
		if err := arity(3); err != nil {
			return nil, err
		}
		input, err := text(0)
		if err != nil {
			return nil, err
		}
		pattern, err := text(1)
		if err != nil || len(pattern) > 4096 {
			return nil, errors.New("invalid image extraction pattern")
		}
		group, err := imageInteger(args[2])
		if err != nil {
			return nil, err
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, errors.New("invalid image extraction pattern")
		}
		if group < 0 || group > int64(compiled.NumSubexp()) {
			return nil, errors.New("invalid image extraction group")
		}
		matches := compiled.FindStringSubmatch(input)
		if len(matches) == 0 {
			return nil, errImageValueAbsent
		}
		return matches[group], nil
	case "html-unescape":
		if err := arity(1); err != nil {
			return nil, err
		}
		input, err := text(0)
		if err != nil {
			return nil, err
		}
		return html.UnescapeString(input), nil
	case "get":
		if err := arity(2); err != nil {
			return nil, err
		}
		pointer, err := text(1)
		if err != nil {
			return nil, err
		}
		return e.pointer(args[0], pointer)
	case "flatten":
		if err := arity(1); err != nil {
			return nil, err
		}
		items, ok := args[0].([]any)
		if !ok {
			return nil, errors.New("image flatten requires an array")
		}
		result := []any{}
		for _, item := range items {
			children, ok := item.([]any)
			if !ok {
				return nil, errors.New("image flatten requires nested arrays")
			}
			if len(result)+len(children) > 4096 {
				return nil, errors.New("image collection exceeds limit")
			}
			result = append(result, children...)
		}
		return result, nil
	case "equal":
		if err := arity(2); err != nil {
			return nil, err
		}
		a, aErr := json.Marshal(args[0])
		b, bErr := json.Marshal(args[1])
		if aErr != nil || bErr != nil {
			return nil, errors.New("image equality encoding failed")
		}
		return bytes.Equal(a, b), nil
	case "and", "or":
		result := value.Op == "and"
		for _, arg := range args {
			flag, ok := arg.(bool)
			if !ok {
				return nil, errors.New("image boolean operation requires boolean values")
			}
			if value.Op == "and" {
				result = result && flag
			} else {
				result = result || flag
			}
		}
		return result, nil
	case "not":
		if err := arity(1); err != nil {
			return nil, err
		}
		flag, ok := args[0].(bool)
		if !ok {
			return nil, errors.New("image not requires a boolean")
		}
		return !flag, nil
	case "length":
		if err := arity(1); err != nil {
			return nil, err
		}
		switch input := args[0].(type) {
		case []any:
			return len(input), nil
		case string:
			return len(input), nil
		case []byte:
			return len(input), nil
		case map[string]any:
			return len(input), nil
		}
		return nil, errors.New("image length requires a collection")
	case "integer":
		if err := arity(1); err != nil {
			return nil, err
		}
		return imageInteger(args[0])
	case "first":
		if err := arity(1); err != nil {
			return nil, err
		}
		items, ok := args[0].([]any)
		if !ok || len(items) == 0 {
			return nil, errors.New("image selection has no items")
		}
		return items[0], nil
	default:
		return nil, fmt.Errorf("unsupported image expression operation %q", value.Op)
	}
}

func imageCloneContext(context map[string]any) map[string]any {
	result := make(map[string]any, len(context))
	for key, value := range context {
		result[key] = value
	}
	return result
}
