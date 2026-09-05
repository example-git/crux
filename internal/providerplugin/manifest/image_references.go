package manifest

import "strings"

func validateImageReferences(value ImageManifest, add func(string)) {
	credentials := map[string]bool{}
	for _, credential := range value.Credentials {
		credentials[credential.ID] = true
	}
	for _, workflow := range value.Workflows {
		seen := map[string]bool{}
		var check func(ImageValue, int)
		check = func(expression ImageValue, depth int) {
			if depth > 32 {
				return
			}
			parts := strings.Split(expression.Ref, "/")
			if len(parts) >= 3 {
				name := strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[2])
				switch parts[1] {
				case "steps":
					if !seen[name] {
						add("/workflows/value/ref")
					}
				case "credentials":
					if !credentials[name] {
						add("/workflows/value/ref")
					}
				case "clients":
					if _, ok := value.ClientIdentities[name]; !ok {
						add("/workflows/value/ref")
					}
				}
			}
			for _, child := range expression.Args {
				check(child, depth+1)
			}
			for _, child := range expression.Object {
				check(child, depth+1)
			}
			for _, child := range expression.Array {
				check(child, depth+1)
			}
		}
		for _, step := range workflow.Steps {
			if step.Value != nil {
				check(*step.Value, 0)
			}
			if step.Assert != nil {
				check(*step.Assert, 0)
			}
			for _, binding := range step.Bindings {
				check(binding, 0)
			}
			if request := step.Request; request != nil {
				check(request.URL, 0)
				if request.Body != nil {
					check(*request.Body, 0)
				}
				for _, field := range request.Headers {
					check(field, 0)
				}
				for _, field := range request.Query {
					check(field, 0)
				}
			}
			seen[step.ID] = true
		}
		check(workflow.Result, 0)
	}
	depths := map[string]int{}
	visiting := map[string]bool{}
	var depth func(string) int
	depth = func(id string) int {
		if visiting[id] {
			return 34
		}
		if result := depths[id]; result != 0 {
			return result
		}
		visiting[id] = true
		result := 1
		for _, step := range value.Workflows[id].Steps {
			if step.Call != "" {
				result = max(result, 1+depth(step.Call))
			}
		}
		visiting[id] = false
		depths[id] = result
		return result
	}
	for id := range value.Workflows {
		if depth(id) > 33 {
			add("/workflows/depth")
		}
	}
}
