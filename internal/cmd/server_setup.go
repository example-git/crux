package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/daemon"
	"github.com/example-git/crux/internal/server"
	"github.com/spf13/cobra"
)

var (
	serverSetupHost           string
	serverSetupAdvertise      string
	serverSetupManager        string
	serverSetupWorkspaceRoots []string
	serverSetupForeground     bool
	serverSetupTTL            time.Duration
)

var serverSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Set up and pair a new authenticated Crux server",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		hostURL, err := server.ParseHostURL(serverSetupHost)
		if err != nil {
			return fmt.Errorf("invalid setup host: %w", err)
		}
		if err := validateServerSetupAddresses(hostURL, serverSetupAdvertise); err != nil {
			return err
		}
		if serverSetupTTL <= 0 {
			return errors.New("--enrollment-ttl must be positive")
		}
		if runtime.GOOS == "linux" && !serverSetupForeground {
			if status, statusErr := daemon.Status(cmd.Context()); statusErr == nil {
				return fmt.Errorf("Crux server service is already installed at %s", status.Metadata.Path)
			} else if !errors.Is(statusErr, daemon.ErrServiceNotInstalled) {
				return statusErr
			}
		}
		probe := server.NewServer(nil, hostURL.Scheme, hostURL.Host)
		if err := probe.SetWorkspaceRoots(serverSetupWorkspaceRoots); err != nil {
			return fmt.Errorf("configure workspace roots: %w", err)
		}
		if _, err := connection.EnsureServerIdentity(cmd.Context()); err != nil {
			return err
		}
		enrollment, err := connection.StartEnrollment(cmd.Context(), serverSetupHost, serverSetupAdvertise, serverSetupTTL)
		if err != nil {
			return err
		}
		defer enrollment.Close()
		cmd.Printf("Waiting for one client registration at %s.\n\nRun this command on the client:\n  crux connections pair NAME %s\n\n", enrollment.Address(), enrollment.SetupCode())
		result, err := enrollment.Wait(cmd.Context())
		if err != nil {
			return fmt.Errorf("client enrollment failed: %w", err)
		}
		if err := enrollment.Close(); err != nil {
			return fmt.Errorf("close enrollment listener: %w", err)
		}
		cmd.Printf("Authorized client %s.\n", result.Name)
		if runtime.GOOS == "linux" && !serverSetupForeground {
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve Crux executable: %w", err)
			}
			if resolved, err := filepath.EvalSymlinks(executable); err == nil {
				executable = resolved
			}
			installed, err := daemon.Install(cmd.Context(), daemon.Manager(serverSetupManager), executable, serverSetupHost, serverSetupWorkspaceRoots)
			if err != nil {
				return fmt.Errorf("client was authorized, but service installation failed: %w; recover with `crux server daemon install --host %s`", err, serverSetupHost)
			}
			cmd.Printf("Installed and started the Crux server with %s: %s\n", installed.Manager, installed.Path)
			return nil
		}
		cmd.Println("Pairing complete. Starting the server in the foreground.")
		return runServer(cmd, serverSetupHost, serverSetupWorkspaceRoots)
	},
}

func validateServerSetupAddresses(hostURL *url.URL, advertisedAddress string) error {
	if hostURL.Scheme != "tcp" {
		return errors.New("server setup requires a TCP host")
	}
	if hostURL.Path != "" || hostURL.Hostname() == "" || hostURL.Port() == "" {
		return errors.New("server setup --host must use tcp://HOST:PORT")
	}
	listenPort, err := strconv.Atoi(hostURL.Port())
	if err != nil || listenPort < 0 || listenPort > 65535 {
		return fmt.Errorf("server setup --host has an invalid port: %s", hostURL.String())
	}
	if listenPort == 0 {
		return errors.New("server setup does not support port 0; choose the final server port explicitly")
	}
	if server.IsLoopbackHost(hostURL) {
		return errors.New("server setup does not support loopback TCP hosts because the final loopback server does not require mutual TLS; use a non-loopback host")
	}
	if wildcardHost(hostURL.Hostname()) && advertisedAddress == "" {
		return errors.New("--advertise tcp://HOST:PORT is required when --host uses a wildcard address")
	}
	if advertisedAddress == "" {
		return nil
	}
	normalized, err := connection.NormalizeConnectionAddress(advertisedAddress)
	if err != nil {
		return fmt.Errorf("invalid --advertise address: %w", err)
	}
	advertisedURL, err := server.ParseHostURL(normalized)
	if err != nil {
		return fmt.Errorf("invalid --advertise address: %w", err)
	}
	if server.IsLoopbackHost(advertisedURL) {
		return errors.New("--advertise must use a non-loopback address for authenticated remote connections")
	}
	if advertisedURL.Port() != strconv.Itoa(listenPort) {
		return errors.New("--advertise must use the same port as --host so enrollment and the final server endpoint stay consistent")
	}
	return nil
}

func wildcardHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "" || (ip != nil && ip.IsUnspecified())
}

func init() {
	serverSetupCmd.Flags().StringVar(&serverSetupHost, "host", "tcp://0.0.0.0:9090", "Temporary enrollment and final server listen address")
	serverSetupCmd.Flags().StringVar(&serverSetupAdvertise, "advertise", "", "Reachable tcp://HOST:PORT embedded in the client setup code")
	serverSetupCmd.Flags().StringVar(&serverSetupManager, "manager", string(daemon.ManagerAuto), "Linux service manager: auto, systemd, openrc, or runit")
	serverSetupCmd.Flags().StringArrayVar(&serverSetupWorkspaceRoots, "workspace-root", nil, "Additional server workspace root (repeatable)")
	serverSetupCmd.Flags().BoolVar(&serverSetupForeground, "foreground", false, "Run in the foreground instead of installing a Linux service")
	serverSetupCmd.Flags().DurationVar(&serverSetupTTL, "enrollment-ttl", 10*time.Minute, "One-time enrollment code lifetime")
	serverCmd.AddCommand(serverSetupCmd)
}
