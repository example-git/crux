package cmd

import (
	"context"
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
	daemonLogLines       int
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

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the installed Crux server service status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireLinuxDaemon(); err != nil {
			return err
		}
		status, err := daemon.Status(cmd.Context())
		if err != nil {
			return err
		}
		state := "inactive"
		if status.Active {
			state = "active"
		}
		cmd.Printf("Crux server service: %s\nManager: %s\nPath: %s\n", state, status.Metadata.Manager, status.Metadata.Path)
		if status.Metadata.Host != "" {
			cmd.Printf("Host: %s\n", status.Metadata.Host)
		}
		if !status.Active && status.Detail != "" {
			cmd.Printf("Detail: %s\n", status.Detail)
		}
		return nil
	},
}

var (
	daemonStartCmd   = daemonActionCommand("start", "Start the Crux server service", daemon.Start)
	daemonStopCmd    = daemonActionCommand("stop", "Stop the Crux server service", daemon.Stop)
	daemonRestartCmd = daemonActionCommand("restart", "Restart the Crux server service", daemon.Restart)
)

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the installed Crux server service",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireLinuxDaemon(); err != nil {
			return err
		}
		if err := daemon.Uninstall(cmd.Context()); err != nil {
			return err
		}
		cmd.Println("Uninstalled the Crux server service. Identities and authorized clients were retained.")
		return nil
	},
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show recent Crux server service logs",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireLinuxDaemon(); err != nil {
			return err
		}
		logs, err := daemon.ReadLogs(daemonLogLines)
		if err != nil {
			return err
		}
		if logs != "" {
			cmd.Println(logs)
		}
		return nil
	},
}

func daemonActionCommand(use, short string, action func(context.Context) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLinuxDaemon(); err != nil {
				return err
			}
			if err := action(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("Crux server service %s completed.\n", use)
			return nil
		},
	}
}

func requireLinuxDaemon() error {
	if runtime.GOOS != "linux" {
		return errors.New("server daemon management is only supported on Linux")
	}
	return nil
}

func init() {
	daemonInstallCmd.Flags().StringVar(&daemonHost, "host", "tcp://0.0.0.0:9090", "Authenticated TCP listen address")
	daemonInstallCmd.Flags().StringVar(&daemonManager, "manager", string(daemon.ManagerAuto), "Service manager: auto, systemd, openrc, or runit")
	daemonInstallCmd.Flags().StringArrayVar(&daemonWorkspaceRoots, "workspace-root", nil, "Additional server workspace root (repeatable)")
	daemonLogsCmd.Flags().IntVar(&daemonLogLines, "lines", 100, "Number of recent log lines")
	daemonCmd.AddCommand(daemonInstallCmd, daemonStatusCmd, daemonStartCmd, daemonStopCmd, daemonRestartCmd, daemonUninstallCmd, daemonLogsCmd)
	serverCmd.AddCommand(daemonCmd)
}
