package manifest

import (
	"bytes"
	"embed"
	"errors"
	"io"
	"strings"

	validator "github.com/kaptinlin/jsonschema"
)

//go:embed metaschemas/draft2020-12/*.json metaschemas/draft2020-12/meta/*.json
var builtinMetaschemas embed.FS

// Provider bundles are self-contained declarations. Schema validation must not
// add network requests, mutable remote dependencies or credentials to admission.
func newSchemaCompiler() *validator.Compiler {
	compiler := validator.NewCompiler()
	clear(compiler.Loaders)
	compiler.RegisterLoader("https", func(uri string) (io.ReadCloser, error) {
		const prefix = "https://json-schema.org/draft/2020-12/"
		if !strings.HasPrefix(uri, prefix) {
			return nil, errors.New("external schema references are unavailable in a self-contained provider bundle")
		}
		name := strings.TrimPrefix(uri, prefix)
		switch name {
		case "schema", "meta/core", "meta/applicator", "meta/unevaluated", "meta/validation", "meta/meta-data", "meta/format-annotation", "meta/content":
		default:
			return nil, errors.New("unknown embedded schema reference")
		}
		data, err := builtinMetaschemas.ReadFile("metaschemas/draft2020-12/" + name + ".json")
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	return compiler
}

func compileLocalSchema(data []byte) (*validator.Schema, error) {
	schema, err := newSchemaCompiler().Compile(data)
	if err != nil {
		return nil, err
	}
	if len(schema.UnresolvedReferenceURIs()) != 0 {
		return nil, errors.New("provider schema contains unresolved references; schemas must be self-contained")
	}
	return schema, nil
}
