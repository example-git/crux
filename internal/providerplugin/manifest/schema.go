package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	validator "github.com/kaptinlin/jsonschema"
)

const (
	SchemaID       = "https://raw.githubusercontent.com/example-git/crux/main/provider-plugin.schema.json"
	PresetSchemaID = "https://raw.githubusercontent.com/example-git/crux/main/provider-preset-plugin.schema.json"
)

var (
	compiledProviderSchema = sync.OnceValues(func() (*validator.Schema, error) {
		data, err := SchemaJSON()
		if err != nil {
			return nil, err
		}
		return validator.NewCompiler().Compile(data)
	})
	compiledPresetSchema = sync.OnceValues(func() (*validator.Schema, error) {
		data, err := PresetSchemaJSON()
		if err != nil {
			return nil, err
		}
		return validator.NewCompiler().Compile(data)
	})
)

// Schema returns the deterministic Draft 2020-12 structural schema. Semantic
// validation remains mandatory after structural validation.
func Schema() *jsonschema.Schema {
	reflector := &jsonschema.Reflector{RequiredFromJSONSchemaTags: true}
	schema := reflector.Reflect(&Manifest{})
	schema.ID = jsonschema.ID(SchemaID)
	schema.Title = "Crux Provider Plugin Manifest"
	schema.Description = "Version 1 manifest.json contract for direct *.plugin provider bundles"
	return schema
}

func SchemaJSON() ([]byte, error) {
	data, err := json.MarshalIndent(Schema(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding provider plugin schema: %w", err)
	}
	return append(data, '\n'), nil
}

func validateProviderSchema(data []byte) error {
	locations, err := ProviderSchemaIssuePaths(data)
	if err != nil {
		return err
	}
	if len(locations) == 0 {
		return nil
	}
	return fmt.Errorf("manifest does not conform to the provider plugin schema at %s", strings.Join(locations, ", "))
}

func ProviderSchemaIssuePaths(data []byte) ([]string, error) {
	schema, err := compiledProviderSchema()
	if err != nil {
		return nil, fmt.Errorf("compile provider plugin schema: %w", err)
	}
	return schemaIssuePaths(schema.ValidateJSON(data)), nil
}

func PresetSchemaIssuePaths(data []byte) ([]string, error) {
	schema, err := compiledPresetSchema()
	if err != nil {
		return nil, fmt.Errorf("compile provider preset plugin schema: %w", err)
	}
	return schemaIssuePaths(schema.ValidateJSON(data)), nil
}

func schemaIssuePaths(result *validator.EvaluationResult) []string {
	if result == nil || result.IsValid() {
		return nil
	}
	locations := map[string]struct{}{}
	var visit func(validator.List, string) bool
	visit = func(item validator.List, parent string) bool {
		location := joinSchemaInstanceLocation(parent, item.InstanceLocation)
		hasDetailedError := false
		for _, detail := range item.Details {
			if visit(detail, location) {
				hasDetailedError = true
			}
		}
		hasError := !item.Valid && len(item.Errors) > 0
		if hasError && !hasDetailedError {
			locations[location] = struct{}{}
		}
		return hasError || hasDetailedError
	}
	visit(*result.ToList(true), "")
	values := make([]string, 0, len(locations))
	for location := range locations {
		values = append(values, location)
	}
	sort.Strings(values)
	if len(values) > 32 {
		values = values[:32]
	}
	return values
}

func joinSchemaInstanceLocation(parent, location string) string {
	if location == "" || location == "/" {
		if parent == "" {
			return "/"
		}
		return parent
	}
	if parent == "" || parent == "/" {
		return location
	}
	if location == parent || strings.HasPrefix(location, parent+"/") {
		return location
	}
	return strings.TrimSuffix(parent, "/") + "/" + strings.TrimPrefix(location, "/")
}

func PresetSchema() *jsonschema.Schema {
	reflector := &jsonschema.Reflector{RequiredFromJSONSchemaTags: true}
	schema := reflector.Reflect(&PresetManifest{})
	schema.ID = jsonschema.ID(PresetSchemaID)
	schema.Title = "Crux Provider Preset Plugin Manifest"
	schema.Description = "Version 1 data-only provider preset contract for direct *.plugin bundles"
	return schema
}

func PresetSchemaJSON() ([]byte, error) {
	data, err := json.MarshalIndent(PresetSchema(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding provider preset plugin schema: %w", err)
	}
	return append(data, '\n'), nil
}
