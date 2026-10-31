package localaddon

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/config"
)

func (b *codexNativeBridge) selectPublicModel(ctx context.Context, workspace *codexNativeWorkspace, model, provider string) (config.SelectedModel, error) {
	if provider != "" && provider != "crux" {
		return config.SelectedModel{}, fmt.Errorf("Desktop model provider %q is unavailable; the public provider is crux", provider)
	}
	return b.selectModel(ctx, workspace, model, "")
}

func (b *codexNativeBridge) selectPublicThreadModel(ctx context.Context, binding *codexThreadBinding, model, provider string) (config.SelectedModel, error) {
	if provider != "" && provider != "crux" {
		return config.SelectedModel{}, fmt.Errorf("Desktop model provider %q is unavailable; the public provider is crux", provider)
	}
	return b.selectThreadModel(ctx, binding, model, "")
}
