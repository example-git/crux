// Package main is the entry point for the Crux CLI.
//
//	@title			Crux API
//	@version		1.0
//	@description	Crux is a terminal-based AI coding assistant. This API is served over a Unix socket (or Windows named pipe) and provides programmatic access to workspaces, sessions, agents, LSP, MCP, and more.
//	@contact.name	Crux
//	@contact.url	https://github.com/example-git/crux
//	@license.name	FSL-1.1-MIT
//	@license.url	https://github.com/example-git/crux/blob/main/LICENSE.md
//	@BasePath		/v1
package main

import (
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/example-git/crux/internal/cmd"
	_ "github.com/example-git/crux/internal/dns"
	cruxlog "github.com/example-git/crux/internal/log"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	if err := cruxlog.SetupTraffic(); err != nil {
		slog.Error("Failed to initialize traffic logging", "error", err)
	}
	if os.Getenv("CRUX_PROFILE") != "" {
		go func() {
			slog.Info("Serving pprof at localhost:6060")
			if httpErr := http.ListenAndServe("localhost:6060", nil); httpErr != nil {
				slog.Error("Failed to pprof listen", "error", httpErr)
			}
		}()
	}

	cmd.Execute()
}
