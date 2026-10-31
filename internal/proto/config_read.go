package proto

import (
	"errors"

	"github.com/example-git/crux/internal/config"
)

const MaxConfigReadResponseBytes = 1 << 20

type ConfigReadResponse struct {
	WorkspaceID string                `json:"workspace_id"`
	View        config.ConfigReadView `json:"view"`
}

func DecodeConfigReadResponse(body []byte, workspaceID string) (config.ConfigReadView, error) {
	var response ConfigReadResponse
	if err := decodeProviderAuthJSON(body, MaxConfigReadResponseBytes, &response); err != nil {
		return config.ConfigReadView{}, errors.New("invalid configuration read response")
	}
	if response.WorkspaceID != workspaceID {
		return config.ConfigReadView{}, errors.New("configuration read response does not match request")
	}
	return response.View, response.View.Validate()
}
