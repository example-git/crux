//go:build !windows

package webclient

import "github.com/example-git/crux/internal/terminalhttp"

type terminalExit = terminalhttp.Exit

var (
	terminal            = terminalhttp.Handler
	terminalSessions    = terminalhttp.Sessions
	terminalSize        = terminalhttp.Size
	terminalEnvironment = terminalhttp.Environment
	runTerminal         = terminalhttp.Run
)
