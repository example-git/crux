package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerauth"
)

const (
	MaxProviderAuthRequestBytes  = 16 << 10
	MaxProviderAuthResponseBytes = 1 << 20
)

func DecodeProviderAuthTarget(body []byte) (providerauth.Target, error) {
	var target providerauth.Target
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &target); err != nil {
		return target, err
	}
	return target, target.Validate()
}

func DecodeProviderAuthSnapshot(body []byte) (providerauth.Snapshot, error) {
	var snapshot providerauth.Snapshot
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &snapshot); err != nil {
		return snapshot, err
	}
	fields, err := providerAuthFields(body, "workspace_id", "generation", "providers")
	if err != nil {
		return snapshot, err
	}
	var statuses []json.RawMessage
	if err := json.Unmarshal(fields["providers"], &statuses); err != nil {
		return snapshot, err
	}
	for _, status := range statuses {
		if err := validateProviderAuthStatusFields(status); err != nil {
			return snapshot, err
		}
	}
	return snapshot, snapshot.Validate()
}

func DecodeProviderAccountsState(body []byte) (providerauth.AccountsState, error) {
	var state providerauth.AccountsState
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &state); err != nil {
		return state, err
	}
	fields, err := providerAuthFields(body, "target", "status", "accounts")
	if err != nil {
		return state, err
	}
	if err := validateProviderAuthStatusFields(fields["status"]); err != nil {
		return state, err
	}
	var accounts []json.RawMessage
	if err := json.Unmarshal(fields["accounts"], &accounts); err != nil {
		return state, err
	}
	for _, account := range accounts {
		if _, err := providerAuthFields(account, "id", "display_name", "active", "credential_state", "refreshable"); err != nil {
			return state, err
		}
	}
	return state, state.Validate()
}

func validateProviderAuthStatusFields(body []byte) error {
	fields, err := providerAuthFields(body, "owner", "configured", "disabled", "credentials", "account_state")
	if err != nil {
		return err
	}
	var credentials []json.RawMessage
	if err := json.Unmarshal(fields["credentials"], &credentials); err != nil {
		return err
	}
	for _, credential := range credentials {
		if _, err := providerAuthFields(credential, "kind", "state", "refreshable"); err != nil {
			return err
		}
	}
	return nil
}

func providerAuthFields(body []byte, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid provider authentication object")
	}
	for _, field := range required {
		if value, ok := fields[field]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("provider authentication response is missing %s", field)
		}
	}
	return fields, nil
}

func decodeProviderAuthJSON(body []byte, maximum int, value any) error {
	if len(body) > maximum || validateRuntimeControlJSON(body) != nil {
		return fmt.Errorf("invalid provider authentication JSON")
	}
	if err := validateProviderAuthShape(body, reflect.TypeOf(value).Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode provider authentication JSON: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("provider authentication JSON must contain exactly one value")
	}
	return nil
}

// encoding/json accepts case-insensitive aliases for struct fields and null
// for scalar values. Neither is part of the authentication wire contract.
func validateProviderAuthShape(body []byte, shape reflect.Type) error {
	// Arbitrary JSON data (schemas, literal options and manifest values) retain
	// their keys and null values; the outer lexical validator handles ambiguity.
	if shape.Kind() == reflect.Interface || shape == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return fmt.Errorf("provider authentication fields cannot be null")
	}
	if shape.Kind() == reflect.Pointer {
		return validateProviderAuthShape(body, shape.Elem())
	}
	// csync.Map has custom object JSON and deliberately keeps its internals private.
	if shape == reflect.TypeFor[csync.Map[string, config.ProviderConfig]]() {
		shape = reflect.TypeFor[map[string]config.ProviderConfig]()
	}
	switch shape.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return fmt.Errorf("invalid provider authentication object")
		}
		known := map[string]reflect.Type{}
		required := map[string]bool{}
		providerAuthShapeFields(shape, known, required)
		for name := range required {
			if _, ok := fields[name]; !ok {
				return fmt.Errorf("provider authentication object is missing %s", name)
			}
		}
		for name, raw := range fields {
			field, ok := known[name]
			if !ok {
				return fmt.Errorf("unknown provider authentication field")
			}
			if err := validateProviderAuthShape(raw, field); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(body, &values); err != nil {
			return fmt.Errorf("invalid provider authentication array")
		}
		for _, raw := range values {
			if err := validateProviderAuthShape(raw, shape.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(body, &values); err != nil {
			return fmt.Errorf("invalid provider authentication map")
		}
		for _, raw := range values {
			if err := validateProviderAuthShape(raw, shape.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func providerAuthShapeFields(shape reflect.Type, known map[string]reflect.Type, required map[string]bool) {
	for i := range shape.NumField() {
		field := shape.Field(i)
		if !field.IsExported() {
			continue
		}
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				providerAuthShapeFields(embedded, known, required)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		known[name] = field.Type
		if !strings.Contains(options, "omitempty") && !strings.Contains(options, "omitzero") {
			required[name] = true
		}
	}
}
