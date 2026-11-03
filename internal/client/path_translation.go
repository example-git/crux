package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/example-git/crux/internal/pathtranslate"
)

func (c *Client) LoadPathTranslation(ctx context.Context, workspaceID string, scope pathtranslate.Scope) (pathtranslate.Policy, error) {
	if _, err := scope.Key(); err != nil {
		return pathtranslate.Policy{}, err
	}
	headers, err := c.pathTranslationHeaders(workspaceID)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	query := url.Values{"kind": {scope.Kind}, "session_id": {scope.SessionID}, "provider": {scope.Provider}, "model": {scope.Model}}
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/path-translation", workspaceID), query, headers)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	return decodePathTranslation(rsp)
}

func (c *Client) SavePathTranslation(ctx context.Context, workspaceID string, policy pathtranslate.Policy) (pathtranslate.Policy, error) {
	headers, err := c.pathTranslationHeaders(workspaceID)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/path-translation", workspaceID), nil, jsonBody(policy), headers)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	return decodePathTranslation(rsp)
}

func (c *Client) pathTranslationHeaders(workspaceID string) (http.Header, error) {
	headers := http.Header{"Content-Type": {"application/json"}}
	accepted, err := c.workspaceAttachment(workspaceID, nil)
	if err != nil {
		return nil, err
	}
	if accepted != nil {
		accepted.SetHeaders(headers)
	}
	return headers, nil
}

func decodePathTranslation(rsp *http.Response) (pathtranslate.Policy, error) {
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return pathtranslate.Policy{}, err
	}
	var policy pathtranslate.Policy
	if err := json.NewDecoder(rsp.Body).Decode(&policy); err != nil {
		return pathtranslate.Policy{}, fmt.Errorf("decode path translation settings: %w", err)
	}
	return policy, nil
}
