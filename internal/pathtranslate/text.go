package pathtranslate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

func transformJSONStrings(input string, transform func(string, string) (string, error)) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode translated JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("translated input must contain exactly one JSON value")
	}
	var visit func(string, any) (any, error)
	visit = func(key string, value any) (any, error) {
		switch value := value.(type) {
		case string:
			return transform(key, value)
		case []any:
			for index := range value {
				translated, err := visit(key, value[index])
				if err != nil {
					return nil, err
				}
				value[index] = translated
			}
		case map[string]any:
			for key := range value {
				translated, err := visit(key, value[key])
				if err != nil {
					return nil, err
				}
				value[key] = translated
			}
		}
		return value, nil
	}
	translated, err := visit("", value)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(translated); err != nil {
		return "", err
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

func transformPathTokens(text string, transform func(string) (string, error)) (string, error) {
	var output strings.Builder
	output.Grow(len(text))
	for len(text) > 0 {
		if text[0] == '\'' || text[0] == '"' || text[0] == '`' {
			quote := text[0]
			end := strings.IndexByte(text[1:], quote)
			if end >= 0 {
				value := text[1 : end+1]
				translated, err := transform(value)
				if err != nil {
					return "", err
				}
				if translated == value {
					translated, err = transformPathTokens(value, transform)
					if err != nil {
						return "", err
					}
				}
				output.WriteByte(quote)
				output.WriteString(translated)
				output.WriteByte(quote)
				text = text[end+2:]
				continue
			}
		}
		end := strings.IndexFunc(text, func(char rune) bool {
			return unicode.IsSpace(char) || strings.ContainsRune("\"'`<>[]{}(),;|=&:", char)
		})
		if end == 0 {
			output.WriteByte(text[0])
			text = text[1:]
			continue
		}
		if end < 0 {
			end = len(text)
		}
		translated, err := transform(text[:end])
		if err != nil {
			return "", err
		}
		output.WriteString(translated)
		text = text[end:]
	}
	return output.String(), nil
}
