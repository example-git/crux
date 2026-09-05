package providerregistry

import (
	"fmt"
	"maps"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	declarativetransport "github.com/example-git/crux/internal/providertransport/declarative"
)

func IsResponseVerbosityControl(control manifest.RuntimeControl) bool {
	return strings.Contains(strings.ToLower(control.ID+" "+control.RequestPath), "response_verbosity")
}

func IsAnalysisEffortControl(control manifest.RuntimeControl) bool {
	semantic := strings.ToLower(control.ID + " " + control.RequestPath)
	return strings.Contains(semantic, "analysis_effort") || strings.Contains(semantic, "reasoning_effort") || strings.Contains(semantic, "reasoning/effort")
}

func declarativeRuntimeCapability(providerID string, construction Construction, controls []manifest.RuntimeControl) *RuntimeCapability {
	return &RuntimeCapability{
		Available: func(string) bool { return true },
		Apply: func(values RuntimeValues, options fantasy.ProviderOptions) fantasy.ProviderOptions {
			result := maps.Clone(options)
			if result == nil {
				result = fantasy.ProviderOptions{}
			}
			controlsByPath := map[string]any{}
			for _, control := range controls {
				switch {
				case IsResponseVerbosityControl(control) && values.ResponseVerbosity != "":
					controlsByPath[control.RequestPath] = values.ResponseVerbosity
				case IsAnalysisEffortControl(control) && values.AnalysisEffort != "":
					controlsByPath[control.RequestPath] = values.AnalysisEffort
				}
			}
			if construction == ConstructionOpenAIResponses {
				native := &openai.ResponsesProviderOptions{}
				if current, ok := result[openai.Name].(*openai.ResponsesProviderOptions); ok {
					clone := *current
					native = &clone
				}
				native.RuntimeControls = maps.Clone(native.RuntimeControls)
				if native.RuntimeControls == nil {
					native.RuntimeControls = map[string]any{}
				}
				maps.Copy(native.RuntimeControls, controlsByPath)
				if len(native.RuntimeControls) > 0 {
					result[openai.Name] = native
				}
				return result
			}
			declarative := &declarativetransport.Options{Controls: controlsByPath}
			if current, ok := result[providerID].(*declarativetransport.Options); ok {
				declarative.Values = maps.Clone(current.Values)
				maps.Copy(declarative.Controls, current.Controls)
				maps.Copy(declarative.Controls, controlsByPath)
			}
			if len(declarative.Values) > 0 || len(declarative.Controls) > 0 {
				result[providerID] = declarative
			}
			return result
		},
	}
}

func declarativeReasoningCapability(providerID string, construction Construction, controls []manifest.RuntimeControl) *ReasoningCapability {
	return &ReasoningCapability{
		Options: func(_ string, effort string, canReason bool, merged map[string]any) (fantasy.ProviderOptions, error) {
			values := maps.Clone(merged)
			controlsByPath := map[string]any{}
			for _, control := range controls {
				keys := []string{control.ID}
				if index := strings.LastIndexAny(control.ID, "."); index >= 0 {
					keys = append(keys, control.ID[index+1:])
				}
				for _, key := range keys {
					if value, ok := values[key]; ok {
						controlsByPath[control.RequestPath] = value
						delete(values, key)
						break
					}
				}
			}
			if canReason && effort != "" {
				mapped := false
				for _, control := range controls {
					semantic := strings.ToLower(control.ID + " " + control.RequestPath)
					if strings.Contains(semantic, "reasoning") || strings.Contains(semantic, "analysis_effort") {
						if _, configured := controlsByPath[control.RequestPath]; !configured {
							controlsByPath[control.RequestPath] = effort
						}
						mapped = true
					}
				}
				if !mapped {
					values["reasoning_effort"] = effort
				}
			}
			if len(values) == 0 && len(controlsByPath) == 0 {
				return fantasy.ProviderOptions{}, nil
			}
			if construction == ConstructionOpenAIResponses {
				native, err := openai.ParseResponsesOptions(values)
				if err != nil {
					return nil, fmt.Errorf("parse OpenAI Responses provider options: %w", err)
				}
				native.RuntimeControls = controlsByPath
				return fantasy.ProviderOptions{openai.Name: native}, nil
			}
			return fantasy.ProviderOptions{providerID: &declarativetransport.Options{Values: values, Controls: controlsByPath}}, nil
		},
	}
}
