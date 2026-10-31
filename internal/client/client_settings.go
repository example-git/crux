package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/example-git/crux/internal/config"
)

func (c *Client) WriteClientSettings(ctx context.Context, request config.ClientSettingsWriteRequest) (config.ClientSettingsWriteResult, error) {
	if err := request.Validate(); err != nil {
		return config.ClientSettingsWriteResult{}, err
	}
	if store := c.LocalRuntimeStore(); store != nil {
		return store.WriteClientSettings(ctx, request)
	}
	response, err := c.post(ctx, "/client-settings", nil, jsonBody(request), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return config.ClientSettingsWriteResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadRequest {
		return config.ClientSettingsWriteResult{}, errors.Join(config.ErrClientSettingsValidation, checkStatus(response))
	}
	if response.StatusCode == http.StatusConflict {
		return config.ClientSettingsWriteResult{}, config.ErrClientSettingsVersionConflict
	}
	if response.StatusCode == http.StatusForbidden {
		return config.ClientSettingsWriteResult{}, errors.Join(config.ErrClientSettingsReadonly, checkStatus(response))
	}
	if err := checkStatus(response); err != nil {
		return config.ClientSettingsWriteResult{}, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, config.MaxClientSettingsBytes+1))
	if err != nil {
		return config.ClientSettingsWriteResult{}, err
	}
	if len(body) > config.MaxClientSettingsBytes {
		return config.ClientSettingsWriteResult{}, errors.New("client settings response exceeds the supported size")
	}
	var result config.ClientSettingsWriteResult
	if err := json.Unmarshal(body, &result); err != nil {
		return config.ClientSettingsWriteResult{}, err
	}
	if !filepath.IsAbs(result.FilePath) || !strings.HasPrefix(result.Version, "sha256:") {
		return config.ClientSettingsWriteResult{}, errors.New("client settings response is incomplete")
	}
	return result, ctx.Err()
}
