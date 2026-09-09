package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"io"
	"net/http"
	"net/url"
)

func (c *Client) RepairLocalAuthentication(ctx context.Context, id string, request providerauth.LocalRepairRequest) (proto.ProviderLocalRepairResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderLocalRepairResponse{}, err
	}
	if request.WorkspaceID != id {
		return proto.ProviderLocalRepairResponse{}, errors.New("local repair workspace changed")
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > proto.MaxProviderAuthRequestBytes {
		return proto.ProviderLocalRepairResponse{}, errors.New("invalid local repair request")
	}
	response, err := c.fixedAPIKeyRouteClient().sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/local-repair", nil, bytes.NewReader(raw), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderLocalRepairResponse{}, err
	}
	defer response.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderLocalRepairResponse{}, err
	}
	result, err := proto.DecodeProviderLocalRepairResponse(raw, request)
	if err != nil {
		return proto.ProviderLocalRepairResponse{}, err
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderLocalRepairResponse{}, errors.New("local repair HTTP status disagrees with its response")
	}
	if result.Error != nil {
		return result, result.Error
	}
	return result, nil
}
