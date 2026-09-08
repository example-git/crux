package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/version"
)

const maxRemoteRequestBytes = config.MaxRemoteRuntimeBytes + (1 << 20)

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
	decoder := json.NewDecoder(bytes.NewReader(data))
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
