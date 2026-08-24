package server

import "net/http"

func (c *controllerV1) handleGetPlugins(w http.ResponseWriter, r *http.Request) {
	snapshot, err := c.backend.PluginSnapshot()
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, snapshot)
}
