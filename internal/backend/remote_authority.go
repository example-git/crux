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
		if args.Runtime == nil {
			return errors.New("client authority requires a complete runtime")
		}
		if args.AuthenticatedPrincipal == "" && !args.LocalClientAuthority {
			return errors.New("client authority requires verified TLS or local transport")
		}
		if args.AuthenticatedPrincipal != "" && args.LocalClientAuthority {
			return errors.New("local client authority cannot include a TLS principal")
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
	if mode != args.AuthorityMode {
		return ErrWorkspaceAuthority
	}
	if mode != "client" {
		if ws.principal != args.AuthenticatedPrincipal {
			return ErrWorkspaceAuthority
		}
		return nil
	}
	if ws.principal == args.AuthenticatedPrincipal {
		accepted := ws.Cfg.RemoteAuthority()
		if accepted == nil || args.Runtime == nil || accepted.Revision != args.Runtime.Revision || accepted.Digest != args.Runtime.Digest {
			return ErrRuntimeConflict
		}
		return nil
	}
	// A distinct principal may join this client-authority workspace only if
	// the primary owner's own accepted proposal explicitly opted in (see
	// config.RemoteRuntimeProposal.AllowSecondaryOwners), and then only by
	// contributing its own disjoint provider/model manifest as a secondary
	// owner; see registerReusedClient/AdmitSecondaryClientAuthority. Its
	// runtime digest self-consistency was already checked by
	// validateWorkspaceAuthority; the disjointness/merge check happens there.
	// Without that explicit opt-in, a distinct principal is flatly rejected
	// here exactly as if no multi-owner support existed at all, preserving
	// workspace isolation between unrelated authenticated clients by
	// default: an unauthorized principal must not be able to attach to, or
	// merely probe the existence of, another principal's live workspace by
	// presenting any runtime proposal, disjoint or not.
	accepted := ws.Cfg.RemoteAuthority()
	if accepted == nil || !accepted.AllowSecondaryOwners {
		return ErrWorkspaceAuthority
	}
	if args.Runtime == nil || args.LocalClientAuthority {
		return ErrRuntimeConflict
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

// Called with b.mu held. The accepted runtime check and new/rearmed claim are
// indivisible with respect to receiver publication, on every creation reuse.
func (b *Backend) registerReusedClient(ws *Workspace, args proto.Workspace, clientID string) error {
	admit := func() error { b.registerClient(ws, clientID); return nil }
	if args.AuthorityMode != "client" {
		return admit()
	}
	if ws.principal == args.AuthenticatedPrincipal {
		err := ws.Cfg.WithRemoteAuthorityAdmission(config.RemoteAuthority{Mode: "client", Principal: args.AuthenticatedPrincipal, Revision: args.Runtime.Revision, Digest: args.Runtime.Digest}, admit)
		if errors.Is(err, config.ErrRemoteRuntimeRevision) {
			return ErrRuntimeConflict
		}
		return err
	}
	// A distinct principal contributing its own disjoint provider/model
	// manifest to this already-accepted client-authority workspace. Usage of
	// its providers stays locked to this principal; see
	// config.RuntimeSnapshot.ProviderOwnerPrincipal.
	if _, err := ws.Cfg.AdmitSecondaryClientAuthority(ws.ctx, args.AuthenticatedPrincipal, *args.Runtime); err != nil {
		return errors.Join(ErrRuntimeConflict, err)
	}
	return admit()
}
