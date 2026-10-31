package localaddon

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/example-git/crux/internal/session"
)

func (b *codexNativeBridge) updateThreadSettings(ctx context.Context, raw json.RawMessage) error {
	var params struct {
		ThreadID string  `json:"threadId"`
		Model    *string `json:"model"`
		CWD      string  `json:"cwd"`
		Effort   *string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if params.ThreadID == "" {
		return errors.New("threadId is required")
	}
	if params.Effort != nil {
		if err := session.ValidateAnalysisEffort(*params.Effort); err != nil {
			return err
		}
	}
	binding, sess, err := b.findThread(ctx, params.ThreadID, params.CWD)
	if err != nil {
		return err
	}
	previous := binding.model
	if params.Model != nil {
		selection, err := b.selectPublicThreadModel(ctx, binding, *params.Model, "crux")
		if err != nil {
			return err
		}
		if _, err := b.saveSessionModel(ctx, binding.workspace, sess, selection); err != nil {
			binding.model = previous
			return err
		}
	}
	if params.Effort != nil {
		if _, err := binding.workspace.client.SetSessionAnalysisEffort(ctx, binding.workspace.value.ID, binding.nativeID, *params.Effort); err != nil {
			return err
		}
	}
	return nil
}
