package cmd

import (
	"runtime"
	"testing"
	"time"

	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestServerDaemonInstallCommandRegistered(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"server", "daemon", "install"})
	require.NoError(t, err)
	require.Same(t, daemonInstallCmd, command)
	require.NotNil(t, command.Flags().Lookup("host"))
	require.NotNil(t, command.Flags().Lookup("manager"))
	require.NotNil(t, command.Flags().Lookup("workspace-root"))
}

func TestServerDaemonLifecycleCommandsRegistered(t *testing.T) {
	for _, name := range []string{"status", "start", "stop", "restart", "uninstall", "logs"} {
		command, _, err := rootCmd.Find([]string{"server", "daemon", name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
	}
	require.NotNil(t, daemonLogsCmd.Flags().Lookup("lines"))
}

func TestServerSetupRequiresAdvertisedWildcardAddress(t *testing.T) {
	previousHost, previousAdvertise := serverSetupHost, serverSetupAdvertise
	serverSetupHost = "tcp://0.0.0.0:9090"
	serverSetupAdvertise = ""
	t.Cleanup(func() {
		serverSetupHost = previousHost
		serverSetupAdvertise = previousAdvertise
	})

	err := serverSetupCmd.RunE(serverSetupCmd, nil)
	require.EqualError(t, err, "--advertise tcp://HOST:PORT is required when --host uses a wildcard address")
}

func TestServerSetupRejectsLoopbackTCPBeforeEnrollment(t *testing.T) {
	for _, host := range []string{
		"tcp://127.0.0.1:9090",
		"tcp://[::1]:9090",
		"tcp://localhost:9090",
	} {
		t.Run(host, func(t *testing.T) {
			hostURL, err := server.ParseHostURL(host)
			require.NoError(t, err)
			err = validateServerSetupAddresses(hostURL, "")
			require.ErrorContains(t, err, "does not support loopback TCP hosts")
			require.ErrorContains(t, err, "mutual TLS")
		})
	}
}

func TestServerSetupRejectsPortZeroBeforeEnrollment(t *testing.T) {
	hostURL, err := server.ParseHostURL("tcp://0.0.0.0:0")
	require.NoError(t, err)
	err = validateServerSetupAddresses(hostURL, "tcp://server.example:9090")
	require.EqualError(t, err, "server setup does not support port 0; choose the final server port explicitly")
}

func TestServerSetupValidatesAdvertisedEndpoint(t *testing.T) {
	for _, test := range []struct {
		name      string
		host      string
		advertise string
		errorText string
	}{
		{name: "wildcard with matching reachable endpoint", host: "tcp://0.0.0.0:9090", advertise: "tcp://server.example:9090"},
		{name: "specific remote host needs no override", host: "tcp://192.0.2.10:9090"},
		{name: "wildcard advertise", host: "tcp://0.0.0.0:9090", advertise: "tcp://0.0.0.0:9090", errorText: "wildcard host"},
		{name: "loopback advertise", host: "tcp://0.0.0.0:9090", advertise: "tcp://127.0.0.1:9090", errorText: "non-loopback"},
		{name: "different advertised port", host: "tcp://0.0.0.0:9090", advertise: "tcp://server.example:9091", errorText: "same port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostURL, err := server.ParseHostURL(test.host)
			require.NoError(t, err)
			err = validateServerSetupAddresses(hostURL, test.advertise)
			if test.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.errorText)
			}
		})
	}
}

func TestServerSetupEnrollmentTTLDefaultRemainsTenMinutes(t *testing.T) {
	flag := serverSetupCmd.Flags().Lookup("enrollment-ttl")
	require.NotNil(t, flag)
	require.Equal(t, (10 * time.Minute).String(), flag.DefValue)
}

func TestServerDaemonInstallRejectsNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux contract")
	}
	err := daemonInstallCmd.RunE(daemonInstallCmd, nil)
	require.EqualError(t, err, "server daemon installation is only supported on Linux")
}
