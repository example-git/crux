package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
)

func (b *Backend) WriteClientSettings(ctx context.Context, request config.ClientSettingsWriteRequest) (config.ClientSettingsWriteResult, error) {
	if b.cfg == nil {
		return config.ClientSettingsWriteResult{}, errors.New("global configuration authority is unavailable")
	}
	return b.cfg.WriteClientSettings(ctx, request)
}
