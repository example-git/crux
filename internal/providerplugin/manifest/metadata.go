package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	validator "github.com/kaptinlin/jsonschema"
)

const (
	MaxMetadataSchemaBytes  = 1 << 20
	MaxMetadataPayloadBytes = 1 << 20
)

type MetadataSchemas map[string]*validator.Schema

func CompileMetadataContracts(contracts []MetadataContract) (MetadataSchemas, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	compiled := make(MetadataSchemas, len(contracts))
	schemas := make(map[string]string, len(contracts))
	for _, contract := range contracts {
		data, err := json.Marshal(contract.Schema)
		if err != nil {
			return nil, fmt.Errorf("metadata namespace %q schema: %w", contract.Namespace, err)
		}
		if len(data) > MaxMetadataSchemaBytes {
			return nil, fmt.Errorf("metadata namespace %q schema exceeds %d bytes", contract.Namespace, MaxMetadataSchemaBytes)
		}
		encoded := string(data)
		if previous, exists := schemas[contract.Namespace]; exists && previous != encoded {
			return nil, fmt.Errorf("metadata namespace %q declares conflicting schemas", contract.Namespace)
		}
		schemas[contract.Namespace] = encoded
		compiler := validator.NewCompiler()
		if contract.Schema != nil {
			result, err := compiler.ValidateSchema(data)
			if err != nil {
				return nil, fmt.Errorf("metadata namespace %q schema is invalid: %w", contract.Namespace, err)
			}
			if !result.IsValid() {
				return nil, fmt.Errorf("metadata namespace %q schema is invalid: %s", contract.Namespace, formatValidationErrors(result))
			}
		}
		schema, err := compiler.Compile(data)
		if err != nil {
			return nil, fmt.Errorf("metadata namespace %q schema is invalid: %w", contract.Namespace, err)
		}
		compiled[contract.Namespace] = schema
	}
	return compiled, nil
}

func ValidateMetadataValue(namespace string, schema *validator.Schema, value any) error {
	if schema == nil {
		return fmt.Errorf("metadata namespace %q has no compiled schema", namespace)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	if len(data) > MaxMetadataPayloadBytes {
		return fmt.Errorf("payload exceeds %d bytes", MaxMetadataPayloadBytes)
	}
	result := schema.Validate(value)
	if result.IsValid() {
		return nil
	}
	return fmt.Errorf("schema validation failed: %s", formatValidationErrors(result))
}

func formatValidationErrors(result *validator.EvaluationResult) string {
	errorsByPath := result.DetailedErrors()
	constraints := make([]string, 0, len(errorsByPath))
	seen := make(map[string]struct{}, len(errorsByPath))
	for path := range errorsByPath {
		constraint := publicValidationConstraint(path)
		if _, duplicate := seen[constraint]; duplicate {
			continue
		}
		seen[constraint] = struct{}{}
		constraints = append(constraints, constraint)
	}
	sort.Strings(constraints)
	const maximum = 8
	truncated := len(constraints) > maximum
	if truncated {
		constraints = constraints[:maximum]
	}
	for index := range constraints {
		constraints[index] += " constraint failed"
	}
	if truncated {
		constraints = append(constraints, "additional constraints failed")
	}
	if len(constraints) == 0 {
		return "schema constraint failed"
	}
	return strings.Join(constraints, "; ")
}

func publicValidationConstraint(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		path = path[index+1:]
	}
	switch path {
	case "$id", "$ref", "$schema", "$defs", "additionalProperties", "allOf", "anyOf", "const", "contains", "dependentRequired", "dependentSchemas", "else", "enum", "exclusiveMaximum", "exclusiveMinimum", "format", "if", "items", "maxContains", "maximum", "maxItems", "maxLength", "maxProperties", "minContains", "minimum", "minItems", "minLength", "minProperties", "multipleOf", "not", "oneOf", "pattern", "patternProperties", "prefixItems", "properties", "propertyNames", "required", "then", "type", "unevaluatedItems", "unevaluatedProperties", "uniqueItems":
		return path
	default:
		return "value"
	}
}
