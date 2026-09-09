package server

import "net/http"

func (c *controllerV1) handleGetWorkspaceMCPResources(w http.ResponseWriter, r *http.Request) {
	resources, err := c.backend.MCPResources(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonEncode(w, resources)
}
