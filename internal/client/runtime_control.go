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
)

func (c *Client) RuntimeControlState(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return c.runtimeControl(ctx, id, scope, target, nil, false, false)
}

func (c *Client) SetRuntimeControl(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage) (config.RuntimeControlState, error) {
	return c.runtimeControl(ctx, id, scope, target, value, true, true)
}

func (c *Client) RemoveRuntimeControl(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return c.runtimeControl(ctx, id, scope, target, nil, true, false)
}

func (c *Client) runtimeControl(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage, mutation, set bool) (config.RuntimeControlState, error) {
	if err := proto.ValidateRuntimeControlRequest(&scope, target, value, mutation, set); err != nil {
		return config.RuntimeControlState{}, err
	}
	var request any = proto.RuntimeControlRequest{Scope: &scope, Target: target}
	method := http.MethodDelete
	path := fmt.Sprintf("/workspaces/%s/config/runtime-control", id)
	if !mutation {
		method, path = http.MethodPost, path+"/resolve"
	} else if set {
		method = http.MethodPut
		request = proto.SetRuntimeControlRequest{Scope: &scope, Target: target, Value: value}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return config.RuntimeControlState{}, fmt.Errorf("encode runtime control request: %w", err)
	}
	if len(body) > proto.MaxRuntimeControlRequestBytes {
		return config.RuntimeControlState{}, fmt.Errorf("runtime control request exceeds size limit")
	}
	response, err := c.sendReq(ctx, method, path, nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return config.RuntimeControlState{}, fmt.Errorf("runtime control request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		bounded := *response
		bounded.Body = io.NopCloser(io.LimitReader(response.Body, proto.MaxRuntimeControlRequestBytes))
		return config.RuntimeControlState{}, fmt.Errorf("runtime control request: %w", checkStatus(&bounded))
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxRuntimeControlResponseBytes+1))
	if err != nil {
		return config.RuntimeControlState{}, fmt.Errorf("read runtime control response: %w", err)
	}
	state, err := proto.DecodeRuntimeControlState(body)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := proto.ValidateRuntimeControlAcknowledgement(state, scope, target, value, mutation, set); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	return state, nil
}
