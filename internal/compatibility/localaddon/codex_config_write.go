package localaddon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
)

func (b *codexNativeBridge) writeConfig(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var request config.ClientSettingsWriteRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.Join(config.ErrClientSettingsValidation, err)
	}
	if request.Edits == nil {
		return nil, config.ErrClientSettingsValidation
	}
	var modelID, modelProvider string
	var effort *string
	modelChanged := false
	retained := make([]config.ClientSettingsEdit, 0, len(request.Edits))
	for _, edit := range request.Edits {
		if edit.MergeStrategy != "replace" && edit.MergeStrategy != "upsert" {
			return nil, config.ErrClientSettingsValidation
		}
		switch edit.KeyPath {
		case "model":
			if json.Unmarshal(edit.Value, &modelID) != nil || modelID == "" {
				return nil, errors.Join(config.ErrClientSettingsValidation, errors.New("model must be an advertised routing ID"))
			}
			modelChanged = true
		case "model_provider":
			if json.Unmarshal(edit.Value, &modelProvider) != nil || modelProvider != "crux" {
				return nil, errors.Join(config.ErrClientSettingsValidation, errors.New("the public model provider is crux"))
			}
		case "model_reasoning_effort":
			value := ""
			if string(edit.Value) != "null" && json.Unmarshal(edit.Value, &value) != nil {
				return nil, config.ErrClientSettingsValidation
			}
			if value != "" && session.ValidateAnalysisEffort(value) != nil {
				return nil, errors.Join(config.ErrClientSettingsValidation, errors.New("unsupported model reasoning effort"))
			}
			effort = &value
		default:
			retained = append(retained, edit)
		}
	}
	request.Edits = retained
	request.Model = nil
	if modelChanged || effort != nil || modelProvider != "" {
		workspace, err := b.workspace(b.request.WorkingDir)
		if err != nil {
			return nil, err
		}
		if _, err := b.listModels(ctx, workspace); err != nil {
			return nil, err
		}
		selection := b.selectedModel[workspace]
		if modelChanged {
			var found bool
			selection, found = b.models[workspace][modelID]
			if !found {
				return nil, errors.Join(config.ErrClientSettingsValidation, fmt.Errorf("model %q is not in the Crux model catalog", modelID))
			}
		}
		if effort != nil {
			selection.ReasoningEffort = *effort
		}
		owner, found := b.modelOwners[workspace][selection.Provider]
		if !found {
			return nil, fmt.Errorf("native provider owner is unavailable for %s", selection.Provider)
		}
		request.Model = &config.OwnedSelectedModel{Model: selection, Owner: owner}
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	nativeClient, err := codexClientFactory(b.request.WorkingDir)
	if err != nil {
		return nil, err
	}
	result, err := nativeClient.WriteClientSettings(ctx, request)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok", "version": result.Version, "filePath": result.FilePath, "overriddenMetadata": nil}, nil
}

func writeCodexConfigError(output *protocolOutput, id json.RawMessage, err error) error {
	kind := "configValidationError"
	if !errors.Is(err, config.ErrClientSettingsValidation) && !errors.Is(err, config.ErrClientSettingsVersionConflict) && !errors.Is(err, config.ErrClientSettingsReadonly) && !errors.Is(err, config.ErrClientRuntimeManaged) {
		return writeCodexError(output, id, -32000, err.Error())
	}
	if errors.Is(err, config.ErrClientSettingsVersionConflict) {
		kind = "configVersionConflict"
	} else if errors.Is(err, config.ErrClientSettingsReadonly) || errors.Is(err, config.ErrClientRuntimeManaged) {
		kind = "configLayerReadonly"
	}
	return output.write(map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": -32600, "message": err.Error(), "data": map[string]any{"config_write_error_code": kind}}})
}
