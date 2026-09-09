package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
)

func (c *Client) SetProviderToolingInstructions(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string) (proto.ProviderToolingState, error) {
	return c.mutateProviderTooling(ctx, id, scope, owner, profile, false)
}

func (c *Client) RemoveProviderToolingInstructions(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner) (proto.ProviderToolingState, error) {
	return c.mutateProviderTooling(ctx, id, scope, owner, "", true)
}

func (c *Client) mutateProviderTooling(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string, remove bool) (proto.ProviderToolingState, error) {
	if err := proto.ValidateProviderToolingRequest(&scope, owner, profile, remove); err != nil {
		return proto.ProviderToolingState{}, err
	}
	var request any = proto.ProviderToolingRequest{Scope: &scope, Owner: owner, Profile: profile}
	method := http.MethodPut
	if remove {
		method = http.MethodDelete
		request = proto.RemoveProviderToolingRequest{Scope: &scope, Owner: owner}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return proto.ProviderToolingState{}, fmt.Errorf("encode provider tooling request: %w", err)
	}
	if len(body) > 16<<10 {
		return proto.ProviderToolingState{}, fmt.Errorf("provider tooling request exceeds size limit")
	}
	rsp, err := c.sendReq(ctx, method, fmt.Sprintf("/workspaces/%s/config/provider-tooling", id), nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderToolingState{}, fmt.Errorf("update provider tooling: %w", err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		bounded := *rsp
		bounded.Body = io.NopCloser(io.LimitReader(rsp.Body, 16<<10))
		return proto.ProviderToolingState{}, fmt.Errorf("update provider tooling: %w", checkStatus(&bounded))
	}
	body, err = io.ReadAll(io.LimitReader(rsp.Body, (16<<10)+1))
	if err != nil {
		return proto.ProviderToolingState{}, fmt.Errorf("read provider tooling response: %w", err)
	}
	if len(body) > 16<<10 {
		return proto.ProviderToolingState{}, fmt.Errorf("provider tooling response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response struct {
		Scope   *config.Scope                       `json:"scope"`
		Owner   *providerregistry.RegistrationOwner `json:"owner"`
		Profile *string                             `json:"profile"`
	}
	if err := decoder.Decode(&response); err != nil {
		return proto.ProviderToolingState{}, fmt.Errorf("decode provider tooling response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || response.Scope == nil || response.Owner == nil || response.Profile == nil {
		return proto.ProviderToolingState{}, fmt.Errorf("invalid provider tooling acknowledgement")
	}
	if err := proto.ValidateProviderToolingRequest(response.Scope, *response.Owner, *response.Profile, true); err != nil {
		return proto.ProviderToolingState{}, fmt.Errorf("invalid provider tooling acknowledgement: %w", err)
	}
	state := proto.ProviderToolingState{Scope: *response.Scope, Owner: *response.Owner, Profile: *response.Profile}
	if state.Scope != scope || state.Owner != owner || (!remove && state.Profile != profile) {
		return proto.ProviderToolingState{}, fmt.Errorf("provider tooling acknowledgement changed the requested selection")
	}
	return state, nil
}
