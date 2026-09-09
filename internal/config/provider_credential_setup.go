package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	kjsonschema "github.com/kaptinlin/jsonschema"
)

// Pending setup is limited to absent declared credential properties. An
// unrelated schema violation is never turned into permission to save or probe.
func validateProviderCredentialSetup(snapshot RuntimeSnapshot, provider ProviderConfig) (bool, error) {
	registration, ok := providerDeclaredRegistrationForProvider(snapshot.registry, provider.ID, provider)
	if !ok || registration.Manifest == nil {
		return false, errors.New("credential setup declaration is unavailable")
	}
	missing := map[string]bool{}
	for _, declaration := range registration.Manifest.Capabilities.Credentials {
		if declaration.ConfigProperty == "" || declaration.Kind == "none" {
			continue
		}
		if _, exists := provider.Configuration[declaration.ConfigProperty]; !exists {
			missing[declaration.ConfigProperty] = true
		}
	}
	encoded, err := json.Marshal(registration.Manifest.Configuration.Schema)
	if err != nil {
		return false, errors.New("credential setup schema cannot be encoded")
	}
	schema, err := kjsonschema.NewCompiler().Compile(encoded)
	if err != nil {
		return false, errors.New("credential setup schema is unavailable")
	}
	values := provider.Configuration
	if values == nil {
		values = map[string]any{}
	}
	result := schema.ValidateMap(values)
	pending := providerMissingConfigurationCredentials(snapshot, provider)
	if result.IsValid() || pending && onlyMissingCredentialRequirements(result, missing) {
		return pending, nil
	}
	return false, errors.New("checked provider configuration is invalid")
}

func onlyMissingCredentialRequirements(result *kjsonschema.EvaluationResult, missing map[string]bool) bool {
	if result == nil {
		return false
	}
	if result.IsValid() {
		return true
	}
	found := false
	for keyword, detail := range result.Errors {
		if detail == nil {
			return false
		}
		if keyword == "required" && (result.InstanceLocation == "" || result.InstanceLocation == "#") {
			parameter := "property"
			if detail.Code == "missing_required_properties" {
				parameter = "properties"
			} else if detail.Code != "missing_required_property" {
				return false
			}
			text, ok := detail.Params[parameter].(string)
			if !ok || !onlyDeclaredMissingNames(text, missing) {
				return false
			}
			found = true
			continue
		}
		// Aggregate errors need concrete child failures, all restricted to
		// missing credentials at the root instance. No message text is parsed.
		switch keyword {
		case "allOf", "anyOf", "oneOf", "$ref", "if", "then", "else", "dependentSchemas":
			if len(result.Details) == 0 {
				return false
			}
		default:
			return false
		}
	}
	for _, child := range result.Details {
		if child != nil && !child.IsValid() {
			if !onlyMissingCredentialRequirements(child, missing) {
				return false
			}
			found = true
		}
	}
	return found
}

// The validator supplies quoted exact property names in a finite parameter,
// not an error message. Match against declared names without splitting on
// punctuation that may itself occur in a valid JSON property name.
func onlyDeclaredMissingNames(value string, missing map[string]bool) bool {
	if value == "" || len(value) > 128*132 {
		return false
	}
	seen := map[int]bool{}
	var match func(int) bool
	match = func(offset int) bool {
		if offset == len(value) {
			return true
		}
		if seen[offset] {
			return false
		}
		seen[offset] = true
		for property := range missing {
			name := fmt.Sprintf("'%s'", property)
			if !strings.HasPrefix(value[offset:], name) {
				continue
			}
			next := offset + len(name)
			if next == len(value) {
				return true
			}
			if strings.HasPrefix(value[next:], ", ") && match(next+2) {
				return true
			}
		}
		return false
	}
	return match(0)
}
