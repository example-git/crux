package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/invopop/jsonschema"
	validator "github.com/kaptinlin/jsonschema"
)

const MaxBrandingBytes = 16 * 1024

const BrandingSchemaID = "https://raw.githubusercontent.com/example-git/crux/main/provider-branding.schema.json"

type Branding struct {
	Schema string `json:"$schema,omitempty" jsonschema:"format=uri-reference"`
	Brand
}

var brandingSchemaValidationMu sync.Mutex

var compiledBrandingSchema = sync.OnceValues(func() (*validator.Schema, error) {
	data, err := BrandingSchemaJSON()
	if err != nil {
		return nil, err
	}
	return compileLocalSchema(data)
})

func BrandingSchemaJSON() ([]byte, error) {
	reflector := &jsonschema.Reflector{RequiredFromJSONSchemaTags: true}
	schema := reflector.Reflect(&Branding{})
	schema.ID = jsonschema.ID(BrandingSchemaID)
	schema.Title = "Crux Provider Branding"
	schema.Description = "Optional branding.json for provider and provider-preset bundles; replaces inline branding"
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func DecodeBrandingStrict(data []byte) (Brand, error) {
	if len(data) > MaxBrandingBytes || !utf8.Valid(data) {
		return Brand{}, fmt.Errorf("branding.json must be UTF-8 and at most %d bytes", MaxBrandingBytes)
	}
	schema, err := compiledBrandingSchema()
	if err != nil {
		return Brand{}, err
	}
	brandingSchemaValidationMu.Lock()
	paths := schemaIssuePaths(schema.ValidateJSON(data))
	brandingSchemaValidationMu.Unlock()
	if len(paths) != 0 {
		return Brand{}, fmt.Errorf("branding.json does not conform to its schema at %s", strings.Join(paths, ", "))
	}
	var value Branding
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Brand{}, fmt.Errorf("decode branding.json: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Brand{}, fmt.Errorf("branding.json must contain exactly one object")
	}
	for _, label := range []string{value.Label, value.ShortName} {
		if strings.IndexFunc(label, unicode.IsControl) >= 0 {
			return Brand{}, fmt.Errorf("branding labels must not contain control characters")
		}
	}
	return value.Brand, nil
}
