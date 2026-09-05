package server

import "net/http"

// handleGetPlugins returns the redacted provider plugin snapshot for the host.
//
//	@Summary		Get provider plugins
//	@Tags			system
//	@Produce		json
//	@Success		200	{object}	proto.PluginSnapshot
//	@Failure		500	{object}	proto.Error
//	@Router			/plugins [get]
func (c *controllerV1) handleGetPlugins(w http.ResponseWriter, r *http.Request) {
	snapshot, err := c.backend.PluginSnapshot()
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, snapshot)
}
