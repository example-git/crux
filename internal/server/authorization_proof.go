package server

import (
	"net/http"

	"github.com/example-git/crux/internal/connection"
)

// handleGetAuthorization proves the identity admitted for this request.
//
//	@Summary Current mutually authenticated client identity
//	@Tags connection
//	@Produce json
//	@Success 200 {object} connection.AuthorizationProof
//	@Failure 403 {object} proto.Error
//	@Router /authorization [get]
func (c *controllerV1) handleGetAuthorization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c.server == nil || c.server.clientAuthorization == nil || r.TLS == nil {
		jsonError(w, http.StatusForbidden, connection.ErrClientAuthorization.Error())
		return
	}
	proof, err := c.server.clientAuthorization.Proof(r.Context(), *r.TLS)
	if err != nil {
		jsonError(w, http.StatusForbidden, connection.ErrClientAuthorization.Error())
		return
	}
	jsonEncode(w, proof)
}
