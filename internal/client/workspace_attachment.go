package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

// Only creation/replacement acknowledgements enter this cache. Discovery does
// not silently adopt a newer accepted runtime for an existing caller.
func (c *Client) retainWorkspaceAttachment(id string, authority *config.RemoteAuthority) {
	if authority == nil {
		return
	}
	a := proto.WorkspaceAttachment{Mode: authority.Mode, Revision: authority.Revision, Digest: authority.Digest}
	if a.Validate() != nil {
		return
	}
	c.attachmentMu.Lock()
	defer c.attachmentMu.Unlock()
	if c.attachments == nil {
		c.attachments = make(map[string]proto.WorkspaceAttachment)
	}
	current, exists := c.attachments[id]
	if !exists || current.Mode != a.Mode || current.Revision <= a.Revision {
		c.attachments[id] = a
	}
}

func (c *Client) WorkspaceRequest(ctx context.Context, id, method, suffix string, query url.Values, body io.Reader, authority config.RemoteAuthority) (*http.Response, error) {
	request, err := c.workspaceRequest(ctx, id, method, suffix, query, body, authority)
	if err != nil {
		return nil, err
	}
	return c.h.Do(request)
}

func (c *Client) OpenWorkspaceTerminal(ctx context.Context, id string, query url.Values, authority config.RemoteAuthority) (*websocket.Conn, error) {
	request, err := c.workspaceRequest(ctx, id, http.MethodGet, "/terminal", query, nil, authority)
	if err != nil {
		return nil, err
	}
	if !c.secure && c.network != "unix" && c.network != "npipe" {
		return nil, errors.New("remote terminal requires authenticated TLS or a local socket")
	}
	if c.network == "unix" || c.network == "npipe" {
		request.URL.Host = DummyHost
	}
	request.URL.Scheme = "ws"
	if c.secure {
		request.URL.Scheme = "wss"
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, NetDialContext: c.dialer, TLSClientConfig: c.workspaceChannelTLSConfig()}
	connection, response, err := dialer.DialContext(ctx, request.URL.String(), request.Header)
	if err != nil && response != nil {
		defer response.Body.Close()
		if statusErr := checkStatus(response); statusErr != nil {
			return nil, statusErr
		}
	}
	return connection, err
}

func (c *Client) workspaceRequest(ctx context.Context, id, method, suffix string, query url.Values, body io.Reader, authority config.RemoteAuthority) (*http.Request, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || !strings.HasPrefix(suffix, "/") || path.Clean(suffix) != suffix || strings.ContainsAny(suffix, "\\?#") {
		return nil, errors.New("invalid workspace request path")
	}
	attachment, err := c.workspaceAttachment(id, []config.RemoteAuthority{authority})
	if err != nil {
		return nil, err
	}
	values := make(url.Values, len(query)+1)
	for key, entries := range query {
		values[key] = append([]string(nil), entries...)
	}
	values.Set("client_id", c.clientID)
	headers := http.Header{"Content-Type": {"application/json"}}
	attachment.SetHeaders(headers)
	endpoint := (&url.URL{Path: "/v1/workspaces/" + id + suffix, RawQuery: values.Encode()}).String()
	return c.buildReq(ctx, method, endpoint, body, headers)
}

func (c *Client) workspaceAttachment(id string, authority []config.RemoteAuthority) (*proto.WorkspaceAttachment, error) {
	if len(authority) > 1 {
		return nil, errors.New("only one accepted workspace authority is allowed")
	}
	if len(authority) == 1 {
		a := proto.WorkspaceAttachment{Mode: authority[0].Mode, Revision: authority[0].Revision, Digest: authority[0].Digest}
		return &a, a.Validate()
	}
	c.attachmentMu.RLock()
	a, exists := c.attachments[id]
	c.attachmentMu.RUnlock()
	if exists {
		return &a, nil
	}
	if c.secure {
		return nil, errors.New("workspace attachment requires an acknowledged creation or runtime replacement")
	}
	return nil, nil
}
