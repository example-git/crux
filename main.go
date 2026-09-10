// Package main is the entry point for the Crux CLI.
//
//	@title			Crux API
//	@version		1.0
//	@description	Crux is a terminal-based AI coding assistant. This API is served locally over a Unix socket or Windows named pipe, or remotely over TLS 1.3 with mutual certificate authentication. Authenticated remote workspace routes bind IDs to the verified client principal. Client-owned runtimes negotiate capabilities before private admission; public discovery never supplies private credentials for restoration. Enrollment and authorization administration are separate local operations.
//	@contact.name	Crux
//	@contact.url	https://github.com/example-git/crux
//	@license.name	FSL-1.1-MIT
//	@license.url	https://github.com/example-git/crux/blob/main/LICENSE.md
//	@BasePath		/v1
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/example-git/crux/internal/cmd"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/compatibility/localaddon"
	"github.com/example-git/crux/internal/dns"
	"github.com/joho/godotenv"
)

func registerCompatibility() error {
	return localaddon.Register()
}

func main() {
	if !localaddon.IsCompatibilityExecutable(os.Args[0]) || os.Getenv(compatibility.BypassEnvironment) != "" {
		if handled, err := cmd.ExecuteShellCompletion(os.Args[0], os.Args[1:], os.Stdout, os.Stderr); handled {
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
			return
		}
	}
	// Completion requests must not read local dotenv files or initialize
	// normal application services. Preserve dotenv loading for every other
	// command, including compatibility aliases.
	_ = godotenv.Load()
	dns.Configure()
	cmd.InitializeEnvironmentDefaults()
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
