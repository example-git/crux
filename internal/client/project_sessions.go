package client

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/example-git/crux/internal/session"
)

func (c *Client) ProjectSessions(ctx context.Context, path, dataDir string, offset int) (session.Catalog, error) {
	query := url.Values{"path": {path}, "data_dir": {dataDir}, "offset": {strconv.Itoa(offset)}}
	rsp, err := c.get(ctx, "/project-sessions", query, nil)
	if err != nil {
		return session.Catalog{}, err
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return session.Catalog{}, err
	}
	var result session.Catalog
	if err := json.NewDecoder(rsp.Body).Decode(&result); err != nil {
		return session.Catalog{}, err
	}
	return result, nil
}
