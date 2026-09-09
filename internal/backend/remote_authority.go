package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

var (
	ErrWorkspaceAuthority   = errors.New("workspace belongs to a different authenticated principal or authority mode")
	ErrRuntimeConflict      = errors.New("workspace has a different accepted runtime; use an explicit revision update")
	ErrInvalidClientRuntime = errors.New("invalid client runtime admission")
)

// BindClientPrincipal gives a process UUID no authority of its own. Bindings
// survive retirement to fence delayed requests and cross-principal reuse.
func (b *Backend) BindClientPrincipal(clientID, principal string) error {
	if _, err := validateClientID(clientID); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if lifetime := b.principalLocked(principal); lifetime != nil && lifetime.revoked {
		return ErrPrincipalRevoked
	}
	if b.clientPrincipals == nil {
		b.clientPrincipals = make(map[string]string)
	}
	if owner, ok := b.clientPrincipals[clientID]; ok && owner != principal {
		return ErrWorkspaceAuthority
	}
	b.clientPrincipals[clientID] = principal
	return nil
}

func (b *Backend) AuthorizeWorkspace(id, principal string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if lifetime := b.principals[principal]; lifetime != nil && lifetime.revoked {
		return ErrPrincipalRevoked
	}
	ws, err := b.GetWorkspace(id)
	if err != nil {
		return err
	}
	if ws.principal != principal {
		return ErrWorkspaceAuthority
	}
	return nil
}

func (b *Backend) ListWorkspacesForPrincipal(principal string) []proto.Workspace {
	result := []proto.Workspace{}
	for _, ws := range b.workspaces.Seq2() {
		if ws.principal == principal {
			result = append(result, workspaceToProto(ws))
		}
	}
	return result
}

func validateWorkspaceAuthority(args proto.Workspace) error {
	if args.AuthenticatedPrincipal != "" {
		value, err := hex.DecodeString(args.AuthenticatedPrincipal)
		if err != nil || len(value) != sha256.Size {
			return ErrWorkspaceAuthority
		}
	}
	switch args.AuthorityMode {
	case "client":
		if args.AuthenticatedPrincipal == "" || args.Runtime == nil {
			return errors.New("client authority requires a verified TLS principal and complete runtime")
		}
		digest, err := config.RemoteRuntimeDigest(*args.Runtime)
		if err != nil || digest != args.Runtime.Digest {
			return errors.New("runtime proposal digest mismatch")
		}
	case "server":
		if args.Runtime != nil {
			return errors.New("server authority cannot include a client runtime")
		}
	default:
		return errors.New("explicit client or server authority is required")
	}
	return nil
}

func checkWorkspaceReuse(ws *Workspace, args proto.Workspace) error {
	mode := ws.authorityMode
	if mode == "" {
		mode = "server"
	} // Existing local backend callers.
	if ws.principal != args.AuthenticatedPrincipal || mode != args.AuthorityMode {
		return ErrWorkspaceAuthority
	}
	if mode == "client" {
		accepted := ws.Cfg.RemoteAuthority()
		if accepted == nil || args.Runtime == nil || accepted.Revision != args.Runtime.Revision || accepted.Digest != args.Runtime.Digest {
			return ErrRuntimeConflict
		}
	}
	return nil
}

// The full principal and canonical path are part of durable storage identity.
// Database pooling and locks therefore cannot share a different owner's history.
func remoteWorkspaceDataDir(path, requested, principal string) string {
	if requested == "" {
		return filepath.Join(path, ".crux", "remote", principal)
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(requested, "remote", principal, hex.EncodeToString(sum[:]))
}
