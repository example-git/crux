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
	"context"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/example-git/crux/internal/cmd"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/compatibility/localaddon"
	_ "github.com/example-git/crux/internal/dns"
	cruxlog "github.com/example-git/crux/internal/log"
	_ "github.com/joho/godotenv/autoload"
)

func registerCompatibility() error {
	return localaddon.Register()
}

func main() {
	if err := registerCompatibility(); err != nil {
		slog.Error("Failed to register CLI compatibility adapters", "error", err)
		os.Exit(1)
	}
	invocation := compatibility.Invocation{
		Executable: os.Args[0],
		Args:       os.Args[1:],
		Env:        os.Environ(),
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
	if exitCode, handled, err := localaddon.ForwardIfDisabled(context.Background(), invocation); handled {
		if err != nil {
			slog.Error("Failed to forward disabled compatibility alias", "error", err)
		}
		os.Exit(exitCode)
	}
	if exitCode, handled := compatibility.Dispatch(context.Background(), invocation); handled {
		os.Exit(exitCode)
	}
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
