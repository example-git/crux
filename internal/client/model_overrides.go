package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

// OverrideModels requests an atomic, non-persistent model selection change on a
// server-owned workspace and returns its complete acknowledged model state.
func (c *Client) OverrideModels(ctx context.Context, id string, requested config.AgentModelState) (config.AgentModelState, error) {
	if err := requested.Validate(); err != nil {
		return config.AgentModelState{}, fmt.Errorf("override models: %w", err)
	}
	requestBody, err := json.Marshal(proto.ModelOverridesRequest{State: requested})
	if err != nil {
		return config.AgentModelState{}, fmt.Errorf("encode model overrides: %w", err)
	}
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/config/model-overrides", id), nil, bytes.NewBuffer(requestBody), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return config.AgentModelState{}, fmt.Errorf("override models: %w", err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		bounded := *rsp
		bounded.Body = io.NopCloser(io.LimitReader(rsp.Body, 64<<10))
		return config.AgentModelState{}, fmt.Errorf("override models: %w", checkStatus(&bounded))
	}
	body, err := io.ReadAll(io.LimitReader(rsp.Body, (1<<20)+1))
	if err != nil {
		return config.AgentModelState{}, fmt.Errorf("read model overrides response: %w", err)
	}
	if len(body) > 1<<20 {
		return config.AgentModelState{}, fmt.Errorf("model overrides response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var state config.AgentModelState
	if err := decoder.Decode(&state); err != nil {
		return config.AgentModelState{}, fmt.Errorf("decode model overrides: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return config.AgentModelState{}, fmt.Errorf("model overrides response must contain exactly one model state")
	}
	if err := state.Validate(); err != nil {
		return config.AgentModelState{}, fmt.Errorf("validate model overrides: %w", err)
	}
	for _, selection := range []struct {
		name                    string
		requested, acknowledged *config.OwnedSelectedModel
	}{{"large", requested.Large, state.Large}, {"small", requested.Small, state.Small}} {
		if selection.requested == nil {
			continue
		}
		// Compare the transmitted selection, including arbitrary nested
		// provider options, without depending on callers' Go numeric types.
		expected, err := json.Marshal(selection.requested)
		if err != nil {
			return config.AgentModelState{}, fmt.Errorf("encode requested %s model: %w", selection.name, err)
		}
		actual, err := json.Marshal(selection.acknowledged)
		if err != nil || !equalModelOverrideEncoding(expected, actual) {
			return config.AgentModelState{}, fmt.Errorf("model overrides acknowledgement changed the requested %s model", selection.name)
		}
	}
	return state, nil
}

// Both inputs come from json.Marshal, which orders object keys consistently.
// Token comparison permits equivalent number spellings without accepting
// rounding of exact numeric provider options by a receiver's JSON decoder.
func equalModelOverrideEncoding(expected, actual []byte) bool {
	left, right := json.NewDecoder(bytes.NewReader(expected)), json.NewDecoder(bytes.NewReader(actual))
	left.UseNumber()
	right.UseNumber()
	for {
		a, aerr := left.Token()
		b, berr := right.Token()
		if aerr != nil || berr != nil {
			return aerr == io.EOF && berr == io.EOF
		}
		if a == b {
			continue
		}
		an, aok := a.(json.Number)
		bn, bok := b.(json.Number)
		if !aok || !bok {
			return false
		}
		ar, aok := new(big.Rat).SetString(string(an))
		br, bok := new(big.Rat).SetString(string(bn))
		if !aok || !bok || ar.Cmp(br) != 0 {
			return false
		}
	}
}
