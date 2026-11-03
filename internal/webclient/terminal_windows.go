//go:build windows

package webclient

import "github.com/example-git/crux/internal/terminalhttp"

var (
	terminal         = terminalhttp.Handler
	terminalSessions = terminalhttp.Sessions
)
