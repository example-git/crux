package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/providerregistry"
)

func (s *ConfigStore) RuntimeControlState(ctx context.Context, scope Scope, target RuntimeControlTarget) (RuntimeControlState, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.runtimeControlGuardLocked(ctx); err != nil {
		return RuntimeControlState{}, err
	}
	return s.runtimeControlStateLocked(ctx, scope, target)
}

func (s *ConfigStore) SetRuntimeControl(ctx context.Context, scope Scope, target RuntimeControlTarget, value json.RawMessage) (RuntimeControlState, error) {
	return s.mutateRuntimeControl(ctx, scope, target, value, false)
}

func (s *ConfigStore) RemoveRuntimeControl(ctx context.Context, scope Scope, target RuntimeControlTarget) (RuntimeControlState, error) {
	return s.mutateRuntimeControl(ctx, scope, target, nil, true)
}

func (s *ConfigStore) runtimeControlGuardLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.RemoteAuthority() != nil {
		return ErrClientRuntimeManaged
	}
	return nil
}

func (s *ConfigStore) runtimeControlStateLocked(ctx context.Context, scope Scope, target RuntimeControlTarget) (RuntimeControlState, error) {
	state, err := s.Config().RuntimeControlEffectiveState(scope, target)
	if err != nil {
		return RuntimeControlState{}, err
	}
	path, err := s.configPath(scope)
	if err != nil {
		return RuntimeControlState{}, err
	}
	data, _, err := readRuntimeControlFile(path)
	if err != nil {
		return RuntimeControlState{}, err
	}
	keys, err := runtimeControlStorageKeys(state)
	if err != nil {
		return RuntimeControlState{}, err
	}
	state.Scoped, err = runtimeControlReadField(data, keys)
	if err != nil {
		return RuntimeControlState{}, err
	}
	state.ScopedKnown = true
	if err := s.runtimeControlSourceLocked(ctx, &state); err != nil {
		return RuntimeControlState{}, err
	}
	return state, nil
}

func (s *ConfigStore) mutateRuntimeControl(ctx context.Context, scope Scope, target RuntimeControlTarget, value json.RawMessage, remove bool) (RuntimeControlState, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.runtimeControlGuardLocked(ctx); err != nil {
		return RuntimeControlState{}, err
	}
	if err := target.Validate(true); err != nil {
		return RuntimeControlState{}, err
	}
	current := s.Config()
	state, err := current.RuntimeControlEffectiveState(scope, target)
	if err != nil {
		return RuntimeControlState{}, err
	}
	if !remove {
		registration, _ := current.ProviderRegistration(target.Owner.ProviderID)
		for _, control := range registration.RuntimeControls {
			if control.ID == target.ControlID {
				if err := providerregistry.ValidateRuntimeControlValue(control, value); err != nil {
					return RuntimeControlState{}, err
				}
			}
		}
	}
	path, err := s.configPath(scope)
	if err != nil {
		return RuntimeControlState{}, err
	}
	if s.workingDir == "" {
		return RuntimeControlState{}, errors.New("cannot mutate runtime controls without a working directory")
	}
	keys, err := runtimeControlStorageKeys(state)
	if err != nil {
		return RuntimeControlState{}, err
	}
	before, existed, err := readRuntimeControlFile(path)
	if err != nil {
		return RuntimeControlState{}, err
	}
	after, err := runtimeControlChangeField(before, keys, value, remove)
	if err != nil {
		return RuntimeControlState{}, err
	}
	base := s.baseEnvironment
	if base == nil {
		base = snapshotEnvironment()
	}
	dataDir := ""
	if current.Options != nil {
		dataDir = current.Options.DataDirectory
	}
	preview, err := s.previewScopedConfigWrite(ctx, path, before, existed, after, base, dataDir)
	if err != nil {
		return RuntimeControlState{}, fmt.Errorf("stage runtime control config: %w", err)
	}
	if current.providerScan != nil {
		preview.bindProviderScan(*current.providerScan)
	} else {
		preview.bindProviderScan(ProviderScan{Registry: s.providerRegistry})
	}
	// Provider catalogs may be supplied by the registration rather than the
	// persistence file. Reuse the accepted catalog for that unchanged owner;
	// configured replacement catalogs are retained for selection validation.
	for id, provider := range preview.Providers.Seq2() {
		accepted, exists := current.Providers.Get(id)
		if exists && len(provider.Models) == 0 {
			provider.Models = cloneProviderConfig(accepted).Models
			preview.Providers.Set(id, provider)
		}
	}
	resolved, err := resolveSelectedModels(preview, s.knownProviders)
	if err != nil {
		return RuntimeControlState{}, fmt.Errorf("stage runtime control selected models: %w", err)
	}
	preview.Models[SelectedModelTypeLarge], preview.Models[SelectedModelTypeSmall] = resolved.Large, resolved.Small
	proposed, err := preview.RuntimeControlEffectiveState(scope, target)
	if err != nil {
		return RuntimeControlState{}, fmt.Errorf("staged runtime control: %w", err)
	}
	if !remove && proposed.Binding.Kind != providerregistry.RuntimeControlHostOption {
		registration, _ := preview.ProviderRegistration(target.Owner.ProviderID)
		_, options := runtimeControlMergedOptions(preview, target)
		_, _, keys := providerregistry.ResolveRuntimeControlOptions(registration.RuntimeControls, options)
		if keys[target.ControlID] != target.ControlID {
			return RuntimeControlState{}, fmt.Errorf("runtime control %q canonical key is not mapped to that control; another declaration or pinned option may consume it", target.ControlID)
		}
	}
	if !runtimeControlModelsPreserved(state, proposed) {
		return RuntimeControlState{}, errors.New("runtime control configuration changed unrelated selected-model fields before persistence")
	}
	if !remove && (proposed.RuntimeDependent || !proposed.Effective.Present || !RuntimeControlJSONEqual(proposed.Effective.Value, value)) {
		if proposed.GlobalOverride != nil {
			return RuntimeControlState{}, fmt.Errorf("runtime control %q is overridden by %s=%s; change or remove that explicit global setting first", target.ControlID, proposed.GlobalOverride.ConfigKey, proposed.GlobalOverride.Value)
		}
		return RuntimeControlState{}, fmt.Errorf("%s runtime control %q is shadowed by another configuration layer or pinned run override", scope, target.ControlID)
	}
	if err := ctx.Err(); err != nil {
		return RuntimeControlState{}, err
	}
	if err := s.writeRuntimeControlFile(ctx, scope, before, existed, after); err != nil {
		return RuntimeControlState{}, err
	}
	if err := s.reloadFromDiskLocked(ctx); err != nil {
		return RuntimeControlState{}, fmt.Errorf("runtime control saved but reload failed: %w", err)
	}
	accepted, err := s.runtimeControlStateLocked(ctx, scope, target)
	if err != nil {
		return RuntimeControlState{}, fmt.Errorf("runtime control saved but acknowledgement failed: %w", err)
	}
	if !runtimeControlValuesEqual(accepted.Effective, proposed.Effective) {
		return RuntimeControlState{}, errors.New("runtime control saved but effective value changed during reload")
	}
	expectedModels, _ := json.Marshal(proposed.Models)
	actualModels, _ := json.Marshal(accepted.Models)
	if !RuntimeControlJSONEqual(expectedModels, actualModels) {
		return RuntimeControlState{}, errors.New("runtime control saved but selected model fields changed during reload")
	}
	if remove && accepted.Scoped.Present || !remove && (!accepted.Scoped.Present || !RuntimeControlJSONEqual(accepted.Scoped.Value, value)) {
		return RuntimeControlState{}, errors.New("runtime control saved but scoped value changed before acknowledgement")
	}
	return accepted, nil
}

func runtimeControlValuesEqual(a, b RuntimeControlValue) bool {
	return a.Present == b.Present && (!a.Present || RuntimeControlJSONEqual(a.Value, b.Value))
}

func runtimeControlModelsPreserved(before, after RuntimeControlState) bool {
	if before.Binding.Kind == providerregistry.RuntimeControlModelOption {
		for _, state := range []*RuntimeControlState{&before, &after} {
			model := state.Models.Large
			if state.Target.Selection.ModelType == SelectedModelTypeSmall {
				model = state.Models.Small
			}
			if model == nil {
				return false
			}
			copy := *model
			copy.Model = cloneSelectedModel(copy.Model)
			delete(copy.Model.ProviderOptions, state.Target.ControlID)
			if state.Target.Selection.ModelType == SelectedModelTypeSmall {
				state.Models.Small = &copy
			} else {
				state.Models.Large = &copy
			}
		}
	}
	a, _ := json.Marshal(before.Models)
	b, _ := json.Marshal(after.Models)
	return RuntimeControlJSONEqual(a, b)
}

func readRuntimeControlFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []byte("{}"), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read runtime control configuration: %w", err)
	}
	return data, true, nil
}

func (s *ConfigStore) writeRuntimeControlFile(ctx context.Context, scope Scope, before []byte, existed bool, after []byte) error {
	unlock, err := s.lockConfig(scope)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	actual, exists, err := readRuntimeControlFile(path)
	if err != nil {
		return err
	}
	if exists != existed || !bytes.Equal(actual, before) {
		return errors.New("runtime control configuration changed before persistence")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicWriteFile(path, after, 0o600)
}

func runtimeControlStorageKeys(state RuntimeControlState) ([]string, error) {
	switch state.Binding.Kind {
	case providerregistry.RuntimeControlHostOption:
		key := runtimeControlHostKey(state.Binding.HostOption)
		if key == "" {
			return nil, errors.New("unsupported host runtime option")
		}
		return strings.Split(key, "."), nil
	case providerregistry.RuntimeControlModelOption:
		return []string{"models", string(state.Target.Selection.ModelType), "provider_options", state.Target.ControlID}, nil
	case providerregistry.RuntimeControlProviderOption:
		return []string{"providers", state.Target.Owner.ProviderID, "provider_options", state.Target.ControlID}, nil
	default:
		return nil, errors.New("unsupported runtime control storage binding")
	}
}

func runtimeControlReadField(data []byte, keys []string) (RuntimeControlValue, error) {
	for _, key := range keys {
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil || object == nil {
			return RuntimeControlValue{}, errors.New("runtime control configuration path must contain objects")
		}
		raw, exists := object[key]
		if !exists {
			return RuntimeControlValue{}, nil
		}
		data = raw
	}
	return RuntimeControlValue{Present: true, Value: bytes.Clone(data)}, nil
}

func runtimeControlChangeField(data []byte, keys []string, value json.RawMessage, remove bool) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, errors.New("runtime control configuration path must contain objects")
	}
	key := keys[0]
	if len(keys) == 1 {
		if remove {
			delete(object, key)
		} else {
			object[key] = bytes.Clone(value)
		}
	} else {
		next, exists := object[key]
		if !exists {
			if remove {
				return bytes.Clone(data), nil
			}
			next = []byte("{}")
		}
		updated, err := runtimeControlChangeField(next, keys[1:], value, remove)
		if err != nil {
			return nil, err
		}
		object[key] = updated
	}
	return json.Marshal(object)
}

func (s *ConfigStore) runtimeControlSourceLocked(ctx context.Context, state *RuntimeControlState) error {
	if !state.Effective.Present || state.Source.Kind == "catalog" || state.Source.Kind == "manifest" || s.workingDir == "" {
		return ctx.Err()
	}
	keys := []string{}
	switch state.Source.Kind {
	case "host-option":
		keys = strings.Split(state.Source.Key, ".")
	case "provider":
		keys = []string{"providers", state.Target.Owner.ProviderID, "provider_options", state.Source.Key}
	case "model":
		model := s.Config().Models[state.Target.Selection.ModelType]
		if _, exists := model.ProviderOptions[state.Source.Key]; exists {
			keys = []string{"models", string(state.Target.Selection.ModelType), "provider_options", state.Source.Key}
		} else {
			keys = []string{"models", string(state.Target.Selection.ModelType), state.Source.Key}
		}
		if _, pinned := s.overrides.Models[state.Target.Selection.ModelType]; pinned {
			state.Source.Kind = "run-override"
			return ctx.Err()
		}
	default:
		return ctx.Err()
	}
	base := s.baseEnvironment
	if base == nil {
		base = snapshotEnvironment()
	}
	paths := append(lookupConfigsFromEnvironment(s.workingDir, base), s.workspacePath)
	slices.Reverse(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "" {
			continue
		}
		data, _, err := readRuntimeControlFile(path)
		if err != nil {
			return err
		}
		value, err := runtimeControlReadField(data, keys)
		if err != nil {
			return err
		}
		if !value.Present {
			continue
		}
		if runtimeControlValuesEqual(value, state.Effective) {
			if path == s.globalDataPath {
				scope := ScopeGlobal
				state.Source.Scope = &scope
			} else if path == s.workspacePath {
				scope := ScopeWorkspace
				state.Source.Scope = &scope
			}
		}
		break
	}
	return nil
}
