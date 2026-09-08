package demo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/ui/model"
	"github.com/invopop/jsonschema"
)

func defaultCaptureOptions() model.PreviewOptions {
	return model.PreviewOptions{Cols: 160, Rows: 45, Example: "all", Model: "dummy-coder", Scenario: "working", Modal: "none", Popover: "none", PlanExpanded: true}
}

func registerSchemaHelp(mux *http.ServeMux) {
	for _, entry := range []struct {
		name  string
		title string
		value any
	}{
		{"options", "Preview options", model.PreviewOptions{}},
		{"fixture", "Fixture data", model.PreviewData{}},
		{"frame", "ANSI preview frame", model.PreviewFrame{}},
	} {
		generate := sync.OnceValues(func() ([]byte, error) {
			reflector := &jsonschema.Reflector{RequiredFromJSONSchemaTags: true}
			schema := reflector.Reflect(entry.value)
			schema.Title = entry.title
			schema.Description = "Structural reference generated from this binary's Go types. Runtime catalog choices and semantic validation remain authoritative."
			if entry.name == "options" {
				data, err := json.Marshal(defaultCaptureOptions())
				if err != nil {
					return nil, err
				}
				var defaults map[string]any
				if err := json.Unmarshal(data, &defaults); err != nil {
					return nil, err
				}
				definition := schema.Definitions["PreviewOptions"]
				if definition == nil {
					return nil, fmt.Errorf("missing PreviewOptions schema")
				}
				for name, value := range defaults {
					if property, ok := definition.Properties.Get(name); ok {
						property.Default = value
					}
				}
			}
			return json.MarshalIndent(schema, "", "  ")
		})
		mux.HandleFunc("GET /help/"+entry.name+".schema.json", func(w http.ResponseWriter, r *http.Request) {
			data, err := generate()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/schema+json")
			w.Write(data)
		})
		mux.HandleFunc("GET /help/"+entry.name+".md", func(w http.ResponseWriter, r *http.Request) {
			data, err := generate()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			markdown, err := schemaMarkdown(entry.title, entry.name, data)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Write([]byte(markdown))
		})
	}
}

func schemaMarkdown(title, name string, data []byte) (string, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s: generated schema reference\n\n[Help index](/help/index.md) · [JSON Schema](/help/%s.schema.json)\n\nGenerated from the Go types compiled into this running binary using invopop/jsonschema. Rebuilding automatically updates this reference; no separate generator is required. These tables describe structure, not the complete request validator. Omission rules, runtime catalog choices, and semantic constraints are described in the [screenshot guide](/help/screenshots.md). Interface and raw JSON fields may accept shapes not expressible by reflection; consult the live fixture document.\n\n", title, name)
	if name == "options" {
		out.WriteString("Defaults shown here are the clientless screenshot defaults. POST /api/preview accepts this object directly; POST /api/control wraps it in `state`. Raw ANSI preview requests do not apply the screenshot defaults.\n\n")
	}
	definitions, _ := document["$defs"].(map[string]any)
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition, _ := definitions[name].(map[string]any)
		fmt.Fprintf(&out, "## %s\n\n", name)
		properties, _ := definition["properties"].(map[string]any)
		if len(properties) == 0 {
			encoded, err := json.MarshalIndent(definition, "", "  ")
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&out, "```json\n%s\n```\n\n", encoded)
			continue
		}
		out.WriteString("| JSON field | Schema (types, references, defaults, constraints) |\n| --- | --- |\n")
		fields := make([]string, 0, len(properties))
		for field := range properties {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			encoded, err := json.Marshal(properties[field])
			if err != nil {
				return "", err
			}
			escape := strings.NewReplacer("|", "&#124;", "`", "&#96;", "\n", " ")
			fmt.Fprintf(&out, "| `%s` | `%s` |\n", escape.Replace(field), escape.Replace(string(encoded)))
		}
		out.WriteString("\n")
	}
	return out.String(), nil
}
