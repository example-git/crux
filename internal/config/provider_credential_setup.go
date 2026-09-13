package config

import (
	"encoding/json"
	"errors"
	"strconv"
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
	if result.IsValid() || pending && onlyMissingCredentialRequirements(result, schema, values, missing) {
		return pending, nil
	}
	return false, errors.New("checked provider configuration is invalid")
}

func onlyMissingCredentialRequirements(result *kjsonschema.EvaluationResult, schema *kjsonschema.Schema, values map[string]any, missing map[string]bool) bool {
	if result == nil || schema == nil {
		return false
	}
	if result.IsValid() {
		return true
	}
	if result.InstanceLocation != "" && result.InstanceLocation != "#" {
		property, dependent := strings.CutPrefix(result.EvaluationPath, "/dependentSchemas/")
		if !dependent || result.InstanceLocation != "/"+property {
			return false
		}
	}
	found := false
	for keyword, detail := range result.Errors {
		if detail == nil {
			return false
		}
		if keyword == "required" {
			requiredMissing := false
			for _, name := range schema.Required {
				if _, exists := values[name]; exists {
					continue
				}
				if !missing[name] {
					return false
				}
				requiredMissing = true
			}
			if !requiredMissing {
				return false
			}
			found = true
			continue
		}
		switch keyword {
		case "allOf", "anyOf", "oneOf", "$ref", "if", "then", "else", "dependentSchemas":
			if len(result.Details) == 0 {
				return false
			}
		default:
			return false
		}
	}
	referencePending := schema.ResolvedRef != nil
	for _, child := range result.Details {
		if child == nil || child.EvaluationPath == "/if" {
			continue
		}
		var childSchema *kjsonschema.Schema
		if child.EvaluationPath == "" {
			if referencePending {
				childSchema = schema.ResolvedRef
				referencePending = false
			} else if schema.If != nil && !schema.If.ValidateMap(values).IsValid() {
				childSchema = schema.Else
			}
		} else {
			childSchema = credentialRequirementChildSchema(schema, child.EvaluationPath)
		}
		if !child.IsValid() {
			if !onlyMissingCredentialRequirements(child, childSchema, values, missing) {
				return false
			}
			found = true
		}
	}
	return found
}

func credentialRequirementChildSchema(schema *kjsonschema.Schema, path string) *kjsonschema.Schema {
	keyword, suffix, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	var children []*kjsonschema.Schema
	switch keyword {
	case "allOf":
		children = schema.AllOf
	case "anyOf":
		children = schema.AnyOf
	case "oneOf":
		children = schema.OneOf
	case "if":
		return schema.If
	case "then":
		return schema.Then
	case "else":
		return schema.Else
	case "dependentSchemas":
		return schema.DependentSchemas[suffix]
	default:
		return nil
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index < 0 || index >= len(children) {
		return nil
	}
	return children[index]
}
