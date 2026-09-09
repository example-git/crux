package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/discover"
	providermanifest "github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"
)

var schemaCmd = &cobra.Command{
	Use:    "schema [configuration|provider-plugin|provider-preset-plugin|image-provider-plugin|provider-branding|remote-runtime]",
	Short:  "Generate a JSON schema",
	Long:   "Generate a Crux configuration, provider manifest or private remote runtime wire schema",
	Hidden: true,
	Args:   cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		kind := "configuration"
		if len(args) == 1 {
			kind = args[0]
		}
		if kind == "remote-runtime" {
			bts, err := remoteRuntimeSchemaJSON()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(bts)
			return err
		}
		if kind == "provider-plugin" || kind == "provider-preset-plugin" || kind == "image-provider-plugin" || kind == "provider-branding" {
			var (
				bts []byte
				err error
			)
			switch kind {
			case "provider-branding":
				bts, err = providermanifest.BrandingSchemaJSON()
			case "provider-plugin":
				bts, err = providermanifest.SchemaJSON()
			case "image-provider-plugin":
				bts, err = providermanifest.ImageSchemaJSON()
			case "provider-preset-plugin":
				bts, err = providermanifest.PresetSchemaJSON()
			}
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(bts)
			return err
		}
		if kind != "configuration" {
			return fmt.Errorf("unknown schema %q", kind)
		}
		// Configuration schema generation must be deterministic and must never
		// absorb provider IDs or configuration fields from locally installed
		// bundles. Runtime provider surfaces belong to the UI/API, not the
		// checked-in public schema.
		bts, err := configurationSchemaJSON(nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(bts))
		return nil
	},
}

// remoteRuntimeSchemaJSON describes private proposal fields without loading
// local plugins, credentials or configuration. Admission additionally checks
// the negotiated compiler, exact owners, content digests and destination policy.
func remoteRuntimeSchemaJSON() ([]byte, error) {
	reflector := new(jsonschema.Reflector)
	schema := reflector.Reflect(&config.RemoteRuntimeProposal{})
	schema.ID = "https://raw.githubusercontent.com/example-git/crux/main/remote-workspace-runtime.schema.json"
	schema.Title = "Private client-owned remote workspace runtime"
	schema.Description = "Private admission/update input, never public workspace discovery. Structural schema only: authenticated negotiation, byte budgets, digests, manifest semantics, exact credential owners and permitted destinations are also required by runtime admission."
	return json.MarshalIndent(schema, "", "  ")
}

// setProviderTypeEnum overwrites the provider `type` enum with the live set
// accepted by configuration loading: OpenAI-compatible providers and local
// providers that register an enricher (for example, Ollama and OMLX).
func configurationSchemaJSON(surfaces []providerregistry.Surface) ([]byte, error) {
	reflector := new(jsonschema.Reflector)
	schema := reflector.Reflect(&config.Config{})
	schema.ID = "https://raw.githubusercontent.com/example-git/crux/main/schema.json"
	setProviderTypeEnum(schema)

	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal configuration schema: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("failed to decode configuration schema: %w", err)
	}
	defs, _ := document["$defs"].(map[string]any)
	configDef, _ := defs["Config"].(map[string]any)
	configProperties, _ := configDef["properties"].(map[string]any)
	providers, _ := configProperties["providers"].(map[string]any)
	if providers == nil {
		return nil, fmt.Errorf("configuration schema has no providers object")
	}
	providerProperties, _ := providers["properties"].(map[string]any)
	if providerProperties == nil {
		providerProperties = make(map[string]any)
		providers["properties"] = providerProperties
	}
	for _, surface := range surfaces {
		if len(surface.Configuration) == 0 {
			continue
		}
		configuration := surface.Configuration
		if len(surface.ConfigurationUI) > 0 {
			configuration = surface.Clone().Configuration
			configuration["x-crux-fields"] = surface.ConfigurationUI
		}
		providerProperties[surface.ID] = map[string]any{
			"allOf": []any{
				map[string]any{"$ref": "#/$defs/ProviderConfig"},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"configuration": configuration,
					},
				},
			},
		}
	}

	result, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal configuration schema: %w", err)
	}
	return result, nil
}

func setProviderTypeEnum(schema *jsonschema.Schema) {
	def, ok := schema.Definitions["ProviderConfig"]
	if !ok || def.Properties == nil {
		return
	}
	typeProp, ok := def.Properties.Get("type")
	if !ok {
		return
	}

	types := []string{"openai-compat"}
	types = append(types, discover.RegisteredProviderTypes()...)

	typeProp.Enum = make([]any, len(types))
	for i, t := range types {
		typeProp.Enum[i] = t
	}
}
