package proto

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

const (
	WorkspaceAuthorityModeHeader     = "Crux-Workspace-Authority-Mode"
	WorkspaceAuthorityRevisionHeader = "Crux-Workspace-Authority-Revision"
	WorkspaceAuthorityDigestHeader   = "Crux-Workspace-Authority-Digest"
)

// WorkspaceAttachment identifies the authority already accepted by the client.
// The principal comes exclusively from authenticated transport, never this DTO.
type WorkspaceAttachment struct {
	Mode     string
	Revision uint64
	Digest   string
}

func (a WorkspaceAttachment) Validate() error {
	switch a.Mode {
	case "server":
		if a.Revision == 0 && a.Digest == "" {
			return nil
		}
	case "client":
		digest, err := hex.DecodeString(a.Digest)
		if err == nil && len(digest) == 32 && a.Revision > 0 {
			return nil
		}
	}
	return errors.New("invalid accepted workspace authority")
}

func (a WorkspaceAttachment) SetHeaders(h http.Header) {
	h.Set(WorkspaceAuthorityModeHeader, a.Mode)
	h.Set(WorkspaceAuthorityRevisionHeader, strconv.FormatUint(a.Revision, 10))
	h.Set(WorkspaceAuthorityDigestHeader, a.Digest)
}

func ParseWorkspaceAttachment(h http.Header) (*WorkspaceAttachment, error) {
	mode, revision, digest := h.Values(WorkspaceAuthorityModeHeader), h.Values(WorkspaceAuthorityRevisionHeader), h.Values(WorkspaceAuthorityDigestHeader)
	if len(mode) == 0 && len(revision) == 0 && len(digest) == 0 {
		return nil, nil
	}
	if len(mode) != 1 || len(revision) != 1 || len(digest) != 1 {
		return nil, errors.New("accepted workspace authority headers must occur exactly once")
	}
	value, err := strconv.ParseUint(revision[0], 10, 64)
	if err != nil {
		return nil, errors.New("invalid accepted workspace revision")
	}
	a := &WorkspaceAttachment{Mode: mode[0], Revision: value, Digest: digest[0]}
	return a, a.Validate()
}
