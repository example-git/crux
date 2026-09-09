package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/version"
)

const maxRemoteRequestBytes = config.MaxRemoteRuntimeBytes + (1 << 20)

type authenticatedConnectionKey struct{}

// Admission wraps the entire router, including documentation and unknown
// routes, so keepalive and multiplexed requests cannot reuse a revoked grant.
// Local sockets and explicitly unauthenticated loopback servers retain their
// existing authority model.
func (s *Server) authorizeRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.tlsConfig != nil || s.remoteManagement() {
			if r.TLS == nil {
				jsonError(w, http.StatusForbidden, connection.ErrClientAuthorization.Error())
				return
			}
			incoming := r
			abort := func() {
				if conn, ok := incoming.Context().Value(authenticatedConnectionKey{}).(net.Conn); ok {
					_ = conn.Close()
					return
				}
				controller := http.NewResponseController(w)
				_ = controller.SetReadDeadline(time.Now())
				_ = controller.SetWriteDeadline(time.Now())
				if incoming.Body != nil {
					_ = incoming.Body.Close()
				}
			}
			ctx, done, err := s.clientAuthorization.AdmitRequest(r.Context(), *r.TLS, abort)
			if err != nil {
				jsonError(w, http.StatusForbidden, connection.ErrClientAuthorization.Error())
				return
			}
			defer done()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func requestPrincipal(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	peer := r.TLS.PeerCertificates[0]
	if !bytes.Equal(peer.Raw, r.TLS.VerifiedChains[0][0].Raw) {
		return ""
	}
	sum := sha256.Sum256(peer.Raw)
	return hex.EncodeToString(sum[:])
}

// Registered inside the ServeMux, after route variables have been extracted.
// Every workspace route (including SSE and tasks) passes this same boundary.
func (s *Server) authorizeRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal := requestPrincipal(r)
		if (s.tlsConfig != nil || s.remoteManagement()) && principal == "" {
			jsonError(w, http.StatusForbidden, "request requires verified client TLS")
			return
		}
		if id := r.PathValue("id"); id != "" {
			if err := s.backend.AuthorizeWorkspace(id, principal); err != nil {
				status := http.StatusForbidden
				if errors.Is(err, backend.ErrWorkspaceNotFound) {
					status = http.StatusNotFound
				}
				jsonError(w, status, err.Error())
				return
			}
		}
		for _, id := range []string{r.PathValue("client_id"), r.URL.Query().Get("client_id")} {
			if id != "" {
				if err := s.backend.BindClientPrincipal(id, principal); err != nil {
					jsonError(w, http.StatusForbidden, err.Error())
					return
				}
			}
		}
		next(w, r)
	}
}

// handleGetRemoteRuntimeCapabilities documents the workspace authority contract.
//
// @Summary Negotiate client runtime support
// @Description Authenticate with the selected client certificate before sending private state. Returns the exact compiler, principal, limits, sharing policy and disconnect grace.
// @Tags runtime
// @Produce json
// @Success 200 {object} proto.RemoteRuntimeCapabilities
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /runtime-capabilities [get]
func (c *controllerV1) handleGetRemoteRuntimeCapabilities(w http.ResponseWriter, r *http.Request) {
	principal := requestPrincipal(r)
	if principal == "" {
		jsonError(w, http.StatusForbidden, "runtime negotiation requires verified client TLS")
		return
	}
	jsonEncode(w, proto.RemoteRuntimeCapabilities{
		Protocol: proto.RemoteRuntimeProtocol, RuntimeVersion: config.RemoteRuntimeVersion,
		Compiler: config.RemoteRuntimeCompiler, HostVersion: version.Version,
		MaxRequestBytes: maxRemoteRequestBytes, MaxBundles: config.MaxRemoteRuntimeBundles,
		MaxProviders: config.MaxRemoteRuntimeProviders, Principal: principal, WorkspaceSharing: "exclusive-certificate",
		DisconnectGraceMillis: c.backend.DetachGrace().Milliseconds(),
	})
}

func requireRuntimeProtocol(w http.ResponseWriter, r *http.Request) bool {
	if requestPrincipal(r) == "" {
		jsonError(w, http.StatusForbidden, "client runtime requires verified client TLS")
		return false
	}
	if r.Header.Get("Crux-Runtime-Protocol") != proto.RemoteRuntimeProtocol {
		jsonError(w, http.StatusPreconditionRequired, "negotiate client runtime capabilities before sending private state")
		return false
	}
	if r.Header.Get(cruxlog.EphemeralStateHeader) == "" {
		jsonError(w, http.StatusBadRequest, "client runtime must be marked ephemeral")
		return false
	}
	return true
}

func decodeRuntimeRequest(w http.ResponseWriter, r *http.Request, result any) error {
	if r.ContentLength > maxRemoteRequestBytes {
		return errors.New("private runtime request exceeds receiver byte limit")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRemoteRequestBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return errors.New("invalid or oversized private runtime request")
	}
	if err := validateRuntimeJSON(data); err != nil {
		return err
	}
	switch result.(type) {
	case *proto.CreateWorkspaceRequest, *proto.UpdateRemoteRuntimeRequest:
		if err := validateRuntimeProviderFields(data); err != nil {
			return err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return errors.New("invalid or oversized private runtime request")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("private runtime request must contain exactly one JSON value")
	}
	return nil
}

// Reject duplicate keys and excessive nesting before decoding maps or base64
// files. Otherwise encoding/json silently merges/replaces duplicate fields.
func validateRuntimeJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("private runtime JSON must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	remaining := 1_000_000
	var walk func(int) error
	walk = func(depth int) error {
		remaining--
		if depth > 64 || remaining < 0 {
			return errors.New("private runtime JSON exceeds structural limits")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid private runtime JSON")
		}
		delim, nested := token.(json.Delim)
		if !nested {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || len(key) > 1024 || keys[key] {
					return errors.New("duplicate or invalid private runtime JSON field")
				}
				keys[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid private runtime JSON")
		}
		if _, err := decoder.Token(); err != nil {
			return errors.New("invalid private runtime JSON")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("private runtime request must contain exactly one JSON value")
	}
	return nil
}

// Inspect only the private proposal's provider fields. Raw values preserve an
// explicit empty/null profile, which decoding into ProviderConfig's string
// would otherwise silently turn into the omitted/default selection. Using the
// same struct field matching as encoding/json also covers accepted key casing,
// without assigning meaning to similarly named plugin configuration/schema keys.
func validateRuntimeProviderFields(data []byte) error {
	var request struct {
		Runtime struct {
			Providers []struct {
				Config struct {
					ToolingInstructions json.RawMessage `json:"tooling_instructions"`
				} `json:"config"`
			} `json:"providers"`
			ProviderContextInstructions map[string]json.RawMessage `json:"provider_context_instructions"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return errors.New("invalid private runtime provider fields")
	}
	for _, provider := range request.Runtime.Providers {
		raw := provider.Config.ToolingInstructions
		if len(raw) == 0 {
			continue
		}
		var profile string
		if json.Unmarshal(raw, &profile) != nil || (profile != config.ToolingInstructionsCrux && profile != config.ToolingInstructionsNative) {
			return errors.New("explicit private runtime tooling instructions must be crux or native")
		}
	}
	for _, raw := range request.Runtime.ProviderContextInstructions {
		if !validRuntimeInstructionString(raw) {
			return errors.New("private runtime provider instructions must be Unicode strings")
		}
	}
	return nil
}

// encoding/json replaces unpaired UTF-16 surrogate escapes with U+FFFD, even
// when the input bytes are valid UTF-8. Provider instruction text must survive
// admission unchanged; an actual U+FFFD character and valid pairs are allowed.
func validRuntimeInstructionString(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		// The structural decoder has already checked escape syntax and length.
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		switch {
		case value >= 0xdc00 && value <= 0xdfff:
			return false
		case value >= 0xd800 && value <= 0xdbff:
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// handlePutWorkspaceRuntime documents the workspace authority contract.
//
// @Summary Replace the accepted client runtime
// @Description Stages the complete private proposal and atomically publishes only against expected_revision. Rejected proposals preserve accepted authority. The reply contains no credentials or bundle content.
// @Tags runtime
// @Produce json
// @Accept json
// @Param request body proto.UpdateRemoteRuntimeRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Param Crux-Runtime-Protocol header string true "Negotiated protocol: crux-client-runtime-v1"
// @Param X-Crux-Ephemeral-State header string true "Nonempty marker suppressing private request bodies from traffic logs"
// @Success 200 {object} config.RemoteAuthority
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 409 {object} proto.Error "Accepted revision or exact refresh identity changed"
// @Failure 428 {object} proto.Error "Runtime protocol negotiation is required"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/runtime [put]
func (c *controllerV1) handlePutWorkspaceRuntime(w http.ResponseWriter, r *http.Request) {
	if !requireRuntimeProtocol(w, r) {
		return
	}
	var args proto.UpdateRemoteRuntimeRequest
	if err := decodeRuntimeRequest(w, r, &args); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	ws, err := c.backend.GetWorkspace(r.PathValue("id"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	ack, err := ws.Cfg.ReplaceRemoteRuntime(r.Context(), args.Runtime, requestPrincipal(r), args.ExpectedRevision)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrRemoteRuntimeRevision) {
			status = http.StatusConflict
		}
		jsonError(w, status, err.Error())
		return
	}
	// Provider snapshots do not change process-wide MCP configuration.
	ws.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: ws.ID}})
	jsonEncode(w, ack)
}

// handlePostClientRefreshCompletion documents the workspace authority contract.
//
// @Summary Acknowledge a client credential refresh
// @Description Completes the exact retained refresh request after the owning client persists and publishes its result. Replaying that completion never starts another token exchange.
// @Tags runtime
// @Produce json
// @Accept json
// @Param request body config.ClientRefreshCompletion true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Param Crux-Runtime-Protocol header string true "Negotiated protocol: crux-client-runtime-v1"
// @Param X-Crux-Ephemeral-State header string true "Nonempty marker suppressing private request bodies from traffic logs"
// @Success 204 "Exact completion acknowledged"
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 409 {object} proto.Error "Accepted revision or exact refresh identity changed"
// @Failure 428 {object} proto.Error "Runtime protocol negotiation is required"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/runtime/refresh-completion [post]
func (c *controllerV1) handlePostClientRefreshCompletion(w http.ResponseWriter, r *http.Request) {
	if !requireRuntimeProtocol(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var response config.ClientRefreshCompletion
	if err := decodeRuntimeRequest(w, r, &response); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	ws, err := c.backend.GetWorkspace(r.PathValue("id"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	if err := ws.Cfg.CompleteClientRefresh(requestPrincipal(r), response); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
