package localaddon

import (
	"path/filepath"

	"github.com/example-git/crux/internal/config"
)

func codexConfigViewResult(view config.ConfigReadView) map[string]any {
	values := map[string]any{}
	origins := map[string]any{}
	fields := map[string]string{"models.large.model": "model", "models.large.provider": "model_provider", "memories.consolidation_model": "memories.consolidation_model"}
	for native, external := range fields {
		if value, exists := view.Values[native]; exists {
			if external == "memories.consolidation_model" {
				values["memories"] = map[string]any{"consolidation_model": value}
			} else {
				values[external] = value
			}
		}
		if origin, exists := view.Origins[native]; exists {
			name := map[string]any{}
			switch origin.Kind {
			case "system":
				name = map[string]any{"type": "system", "file": origin.Path}
			case "user":
				name = map[string]any{"type": "user", "file": origin.Path, "profile": nil}
			case "project", "workspace":
				name = map[string]any{"type": "project", "dotCodexFolder": filepath.Dir(origin.Path)}
			case "runtime":
				name = map[string]any{"type": "sessionFlags"}
			}
			origins[external] = map[string]any{"name": name, "version": origin.Version}
		}
	}
	if len(view.Desktop) > 0 {
		values["desktop"] = view.Desktop
		for key, origin := range view.DesktopOrigins {
			name := map[string]any{"type": "user", "file": origin.Path, "profile": nil}
			switch origin.Kind {
			case "system":
				name = map[string]any{"type": "system", "file": origin.Path}
			case "project", "workspace":
				name = map[string]any{"type": "project", "dotCodexFolder": filepath.Dir(origin.Path)}
			}
			origins["desktop."+key] = map[string]any{"name": name, "version": origin.Version}
		}
	}
	if provider, exists := view.Values["models.large.provider"]; exists {
		if model, exists := view.Values["models.large.model"]; exists {
			values["model"] = codexModelID(provider, model)
		}
	}
	values["model_provider"] = "crux"
	values["model_providers"] = map[string]any{"crux": map[string]any{"name": "Crux"}}
	return map[string]any{"config": values, "origins": origins}
}
