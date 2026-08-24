package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/daemon"
	"github.com/example-git/crux/internal/server"
	"github.com/spf13/cobra"
)

var (
	daemonHost           string
	daemonManager        string
	daemonWorkspaceRoots []string
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the Crux server service",
}

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start a Linux user service for the Crux server",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if runtime.GOOS != "linux" {
			return errors.New("server daemon installation is only supported on Linux")
		}
		hostURL, err := server.ParseHostURL(daemonHost)
		if err != nil {
			return fmt.Errorf("invalid daemon host: %w", err)
		}
		if hostURL.Scheme != "tcp" || server.IsLoopbackHost(hostURL) {
			return errors.New("server daemon host must be a non-loopback TCP address")
		}
		if _, err := connection.ServerTLSConfig(cmd.Context()); err != nil {
			return fmt.Errorf("network authentication is not ready: %w", err)
		}
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve Crux executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		result, err := daemon.Install(cmd.Context(), daemon.Manager(daemonManager), executable, daemonHost, daemonWorkspaceRoots)
		if err != nil {
			return err
		}
		cmd.Printf("Installed and started the Crux server with %s: %s\n", result.Manager, result.Path)
		return nil
	},
}

func init() {
	daemonInstallCmd.Flags().StringVar(&daemonHost, "host", "tcp://0.0.0.0:9090", "Authenticated TCP listen address")
	daemonInstallCmd.Flags().StringVar(&daemonManager, "manager", string(daemon.ManagerAuto), "Service manager: auto, systemd, openrc, or runit")
	daemonInstallCmd.Flags().StringArrayVar(&daemonWorkspaceRoots, "workspace-root", nil, "Additional server workspace root (repeatable)")
	daemonCmd.AddCommand(daemonInstallCmd)
	serverCmd.AddCommand(daemonCmd)
}
