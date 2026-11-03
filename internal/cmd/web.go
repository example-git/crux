package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/webclient"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newWebCommand())
}

func newWebCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "web", Short: "Run the local Crux web client and execution host", Args: cobra.NoArgs,
		RunE: runWeb,
	}
	cmd.Flags().String("listen", "127.0.0.1:0", "Loopback IP and port (0 selects an available port)")
	cmd.Flags().Bool("json", false, "Print the ready URL as JSON for desktop launchers")
	cmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions")
	cmd.Flags().StringP("session", "s", "", "Open an existing session by ID")
	cmd.Flags().BoolP("continue", "C", false, "Open the most recent session")
	cmd.MarkFlagsMutuallyExclusive("session", "continue")
	return cmd
}

func runWeb(cmd *cobra.Command, _ []string) error {
	if connectionName != "" || cmd.Flags().Changed("connection") || cmd.Flags().Changed("host") {
		return errors.New("crux web owns a local execution host; --host and --connection are not supported")
	}
	_, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	path, err := os.Getwd()
	if err != nil {
		return err
	}
	startup := webclient.Startup{Path: path}
	startup.DataDir, _ = cmd.Flags().GetString("data-dir")
	startup.Debug, _ = cmd.Flags().GetBool("debug")
	startup.YOLO, _ = cmd.Flags().GetBool("yolo")
	startup.Channels, _ = cmd.Flags().GetStringSlice("channels")
	startup.SessionID, _ = cmd.Flags().GetString("session")
	if cmd.Flags().Changed("session") && strings.TrimSpace(startup.SessionID) == "" {
		return errors.New("--session requires a nonempty session ID")
	}
	startup.Continue, _ = cmd.Flags().GetBool("continue")
	address, _ := cmd.Flags().GetString("listen")
	listener, err := webclient.Listen(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	store, err := config.Load(config.GlobalWorkspaceDir(), "", startup.Debug)
	if err != nil {
		return fmt.Errorf("load native Crux settings: %w", err)
	}
	cruxlog.Setup(filepath.Join(config.GlobalCacheDir(), "web", "crux.log"), startup.Debug, os.Stderr)
	native := server.NewServer(store, "tcp", listener.Addr().String())
	native.SetLogger(slog.Default())
	native.Backend().SetPersistent(true)
	defer func() {
		native.Backend().Shutdown()
		for _, summary := range native.Backend().ListWorkspaces() {
			if ws, err := native.Backend().GetWorkspace(summary.ID); err == nil {
				ws.Shutdown()
			}
		}
		_ = native.Close()
	}()
	handler := webclient.Handler(native, listener.Addr().String(), startup)
	defer handler.Close()
	signals := append([]os.Signal{os.Interrupt}, addSignals(nil)...)
	ctx, cancel := signal.NotifyContext(cmd.Context(), signals...)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- native.ServeHandler(listener, handler) }()
	readyURL := "http://" + listener.Addr().String()
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		err = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"url": readyURL})
	} else {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Crux web: %s\nWorkspace: %s\nPress Ctrl+C to stop.\n", readyURL, path)
	}
	if err != nil {
		_ = native.Close()
		return err
	}
	select {
	case err = <-done:
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		err = native.Shutdown(shutdownCtx)
		stop()
		if err != nil {
			_ = native.Close()
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
