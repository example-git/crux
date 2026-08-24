package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

const (
	SchemaID       = "https://raw.githubusercontent.com/example-git/crux/main/provider-plugin.schema.json"
	PresetSchemaID = "https://raw.githubusercontent.com/example-git/crux/main/provider-preset-plugin.schema.json"
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
