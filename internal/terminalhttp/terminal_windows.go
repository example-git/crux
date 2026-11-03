//go:build windows

package terminalhttp

import (
	"net/http"

	"github.com/example-git/crux/internal/backend"
)

func Sessions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "tmux sessions are not available on Windows", http.StatusNotImplemented)
	}
}

func Handler(*backend.Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the interactive terminal is not available on Windows", http.StatusNotImplemented)
	}
}
