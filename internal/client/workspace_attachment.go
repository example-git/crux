package client

import (
	"errors"

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
