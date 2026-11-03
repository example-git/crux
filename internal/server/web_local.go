package server

import (
	"net"
	"net/http"
)

func (s *Server) ServeHandler(ln net.Listener, handler http.Handler) error {
	s.h.Handler = handler
	return s.Serve(ln)
}
