package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
)

var connectionsRevokeForce bool

var connectionsCmd = &cobra.Command{
	Use:   "connections",
	Short: "Manage authenticated network connections",
	RunE: func(cmd *cobra.Command, _ []string) error {
		items, err := connection.List(cmd.Context())
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No saved connections.")
			return nil
		}
		for _, item := range items {
			fmt.Printf("%s\t%s\n", item.Name, item.Address)
		}
		return nil
	},
}

var connectionsServerInitCmd = &cobra.Command{
	Use:   "server-init",
	Short: "Create the server identity and print its public pairing code",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		code, err := connection.EnsureServerIdentity(cmd.Context())
		if err != nil {
			return err
		}
		fmt.Println(code)
		return nil
	},
}

var connectionsAddCmd = &cobra.Command{
	Use:   "add <name> <tcp://host:port> <server-pairing-code>",
	Short: "Save a server and create a client identity",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, clientCode, err := connection.Add(cmd.Context(), args[0], args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("Saved connection %s. Give this public client pairing code to the server owner:\n%s\n", args[0], clientCode)
		return nil
	},
}

var connectionsAuthorizeCmd = &cobra.Command{
	Use:   "authorize <name> <client-pairing-code>",
	Short: "Authorize a client public key on this server",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connection.AuthorizeClient(cmd.Context(), args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("Authorized client %s.\n", args[0])
		return nil
	},
}

var connectionsPairCmd = &cobra.Command{
	Use:   "pair <name> <setup-code>",
	Short: "Pair with a server using a one-time setup code",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		saved, err := connection.Pair(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		if err := waitForPairedServer(cmd.Context(), saved); err != nil {
			return fmt.Errorf("paired and saved connection %s, but the server did not become ready: %w", saved.Name, err)
		}
		cmd.Printf("Paired connection %s with %s.\n", saved.Name, saved.Address)
		return nil
	},
}

var connectionsAuthorizedCmd = &cobra.Command{
	Use:   "authorized",
	Short: "List clients authorized by this server",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		clients, err := connection.ListAuthorizedClients(cmd.Context())
		if err != nil {
			return err
		}
		if len(clients) == 0 {
			cmd.Println("No clients are authorized.")
			return nil
		}
		for _, authorized := range clients {
			cmd.Printf("%s\t%s\n", authorized.Name, authorized.Fingerprint)
		}
		return nil
	},
}

var connectionsRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Revoke a client authorized by this server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		if !connectionsRevokeForce {
			input, ok := cmd.InOrStdin().(*os.File)
			if !ok || !term.IsTerminal(input.Fd()) {
				return errors.New("revocation requires confirmation; rerun with --force in non-interactive use")
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Revoke authorized client %s? [y/N] ", name); err != nil {
				return err
			}
			answer, err := bufio.NewReader(input).ReadString('\n')
			if err != nil {
				return err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				return errors.New("revocation cancelled")
			}
		}
		if err := connection.RevokeClient(cmd.Context(), name); err != nil {
			return err
		}
		cmd.Printf("Revoked client %s. Restart the server to apply the updated trust list.\n", name)
		return nil
	},
}

func waitForPairedServer(ctx context.Context, saved connection.Connection) error {
	pairedClient, err := client.NewAuthenticatedClient("", saved)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		err = pairedClient.Health(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("timed out waiting for the paired server")
}

func init() {
	connectionsRevokeCmd.Flags().BoolVarP(&connectionsRevokeForce, "force", "f", false, "Skip interactive revocation confirmation")
	connectionsCmd.AddCommand(
		connectionsServerInitCmd,
		connectionsAddCmd,
		connectionsAuthorizeCmd,
		connectionsPairCmd,
		connectionsAuthorizedCmd,
		connectionsRevokeCmd,
	)
	rootCmd.AddCommand(connectionsCmd)
}
