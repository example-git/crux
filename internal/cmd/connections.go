package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
)

var (
	connectionsRevokeForce     bool
	connectionsRevokeOperation string
)

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
		if err := connection.AuthorizeClientWithApproval(cmd.Context(), args[0], args[1], commandEnrollmentApprover(cmd)); err != nil {
			return err
		}
		cmd.Printf("Authorized client %s.\n", args[0])
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
		clients, err := connection.ListAuthorizationRecords(cmd.Context())
		if err != nil {
			return err
		}
		if len(clients) == 0 {
			cmd.Println("No authorization records are available.")
			return nil
		}
		cmd.Println("Last-use is historical stored data. Live use covers responding running daemons only; observations are not persisted and are lost when a daemon exits.")
		for _, authorized := range clients {
			state := "not authorized"
			if authorized.Authorized {
				state = "authorized"
			} else if authorized.RevokedAt != nil {
				state = "revoked"
			}
			cmd.Printf("%s\t%s\t%s\tcreated=%s\tapproved=%s\tlast-use=%s\tlive-last-use=%s\tlive-use=%s\trevoked=%s\n", authorized.Name, authorized.Fingerprint, state, authorizationRecordTime(authorized.CreatedAt), authorizationRecordTime(authorized.ApprovedAt), authorizationRecordTime(authorized.LastUsedAt), authorizationRecordTime(authorized.LiveLastUsedAt), authorized.LiveUseState, authorizationRecordTime(authorized.RevokedAt))
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
		if !connectionsRevokeForce && connectionsRevokeOperation == "" {
			input, ok := cmd.InOrStdin().(*os.File)
			if !ok || !term.IsTerminal(input.Fd()) {
				return errors.New("revocation requires confirmation; rerun with --force in non-interactive use")
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Revoke authorized client %s? [y/N] ", name); err != nil {
				return err
			}
			answer, err := readEnrollmentApproval(cmd.Context(), input)
			if err != nil {
				return err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				return errors.New("revocation cancelled")
			}
		}
		outcome, err := connection.RevokeClientWithOutcome(cmd.Context(), name, connectionsRevokeOperation)
		if !outcome.Saved {
			return err
		}
		cmd.Printf("Revoked stored authorization for client %s (%s). Operation: %s.\n", outcome.Name, outcome.Principal, outcome.OperationID)
		if err != nil {
			return fmt.Errorf("live cancellation is not fully acknowledged: %w; retry the same receipt with `crux connections revoke %q --operation %s`", err, outcome.Name, outcome.OperationID)
		}
		if len(outcome.Daemons) == 0 {
			cmd.Println("No live daemon was registered; no live cancellation acknowledgement was received.")
			return nil
		}
		cmd.Printf("All %d registered daemon(s) acknowledged cancellation and joined work for this exact principal and grant.\n", len(outcome.Daemons))
		return nil
	},
}

func waitForPairedServer(ctx context.Context, saved connection.Connection) error {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := connection.ConfirmAuthorization(attemptCtx, saved)
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
	connectionsRevokeCmd.Flags().StringVar(&connectionsRevokeOperation, "operation", "", "Retry acknowledgement of an exact saved revocation operation")
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
