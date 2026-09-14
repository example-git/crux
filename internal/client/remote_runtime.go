package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
)

// NegotiateRemoteRuntime is authenticated and contains no private state. It is
// deliberately called before constructing a secret-bearing request body.
func (c *Client) NegotiateRemoteRuntime(ctx context.Context) (*proto.RemoteRuntimeCapabilities, error) {
	if !c.secure && !c.localRuntimeTransport() {
		return nil, errors.New("client runtime requires a local transport or saved authenticated TLS connection")
	}
	rsp, err := c.get(ctx, "/runtime-capabilities", nil, nil)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		switch rsp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("remote runtime authorization failed (GET /v1/runtime-capabilities); verify the saved connection is still authorized: %w", err)
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return nil, fmt.Errorf("remote runtime capability endpoint is unavailable (GET /v1/runtime-capabilities): %w", err)
		default:
			return nil, fmt.Errorf("remote runtime capability negotiation failed (GET /v1/runtime-capabilities): %w", err)
		}
	}
	var value proto.RemoteRuntimeCapabilities
	if err := json.NewDecoder(io.LimitReader(rsp.Body, 64<<10)).Decode(&value); err != nil {
		return nil, errors.New("invalid remote runtime capabilities")
	}
	sharingValid := value.WorkspaceSharing == proto.RemoteRuntimeLocalSharing && value.Principal == ""
	if c.secure {
		sharingValid = value.WorkspaceSharing == proto.RemoteRuntimeCertificateSharing && len(value.Principal) == 64
	}
	if value.Protocol != proto.RemoteRuntimeProtocol || value.RuntimeVersion != config.RemoteRuntimeVersion || value.Compiler != config.RemoteRuntimeCompiler || !sharingValid || value.MaxRequestBytes <= 0 || value.MaxBundles <= 0 || value.MaxProviders <= 0 || value.DisconnectGraceMillis < 0 {
		return nil, errors.New("remote runtime capabilities are incompatible; no private state was sent")
	}
	return &value, nil
}

func validateRemoteRuntimeCapabilities(value *proto.RemoteRuntimeCapabilities, proposal *config.RemoteRuntimeProposal) error {
	if proposal == nil {
		return errors.New("client runtime proposal is required")
	}
	if proposal.CodebaseIndex != nil && !value.CodebaseIndex {
		return errors.New("remote server does not advertise client-owned codebase indexing support")
	}
	if len(proposal.Bundles) > value.MaxBundles || len(proposal.Providers) > value.MaxProviders {
		return errors.New("client runtime exceeds remote receiver limits")
	}
	for _, received := range proposal.Bundles {
		bundle, err := providerplugin.ValidateDetachedBundle(received)
		if err != nil {
			return err
		}
		if err := bundle.ValidateHostVersion(value.HostVersion); err != nil {
			return err
		}
	}
	return nil
}

func runtimeHeaders() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Crux-Runtime-Protocol": {proto.RemoteRuntimeProtocol}, cruxlog.EphemeralStateHeader: {"1"}}
}

func (c *Client) ReplaceRemoteRuntime(ctx context.Context, id string, expected uint64, proposal config.RemoteRuntimeProposal) (*config.RemoteAuthority, error) {
	capabilities, err := c.NegotiateRemoteRuntime(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateRemoteRuntimeCapabilities(capabilities, &proposal); err != nil {
		return nil, err
	}
	body := jsonBody(proto.UpdateRemoteRuntimeRequest{ExpectedRevision: expected, Runtime: proposal})
	if body.Len() > capabilities.MaxRequestBytes {
		return nil, errors.New("client runtime exceeds remote request byte limit")
	}
	rsp, err := c.put(ctx, "/workspaces/"+id+"/runtime", nil, body, runtimeHeaders())
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, err
	}
	var ack config.RemoteAuthority
	if err := json.NewDecoder(rsp.Body).Decode(&ack); err != nil {
		return nil, err
	}
	if mismatch := remoteAuthorityMismatch(&ack, capabilities.Principal, &proposal); mismatch != "" {
		return nil, fmt.Errorf("remote runtime acknowledgement does not match the submitted authority: %s", mismatch)
	}
	c.retainWorkspaceAttachment(id, &ack)
	return &ack, nil
}

// Report the failed contract without echoing received or private runtime data.
func remoteAuthorityMismatch(ack *config.RemoteAuthority, principal string, proposal *config.RemoteRuntimeProposal) string {
	if ack == nil {
		return "authority missing"
	}
	var fields []string
	if ack.Mode != "client" {
		fields = append(fields, "mode")
	}
	if ack.Principal != principal {
		fields = append(fields, "principal")
	}
	if ack.Revision != proposal.Revision {
		fields = append(fields, "revision")
	}
	if ack.Digest != proposal.Digest {
		fields = append(fields, "digest")
	}
	if len(fields) == 0 {
		return ""
	}
	return "mismatched fields: " + strings.Join(fields, ", ")
}

func (c *Client) CompleteClientRefresh(ctx context.Context, id string, result config.ClientRefreshCompletion) error {
	rsp, err := c.post(ctx, "/workspaces/"+id+"/runtime/refresh-completion", nil, jsonBody(result), runtimeHeaders())
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	err = checkStatus(rsp, http.StatusNoContent)
	if rsp.StatusCode >= 400 && rsp.StatusCode < 500 {
		return errors.Join(ErrClientRefreshRejected, err)
	}
	return err
}

var ErrClientRefreshRejected = errors.New("client refresh completion was rejected")
