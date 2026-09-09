package cmd

import (
	"errors"
	"time"

	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
)

var connectionsRevocationsCmd = &cobra.Command{
	Use:   "revocations",
	Short: "Inspect retained revocation receipts and history capacity",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		history, err := connection.ListRevocationHistory(cmd.Context())
		if err != nil {
			return err
		}
		cmd.Printf("Active grants: %d; unresolved revocations: %d; reservation budget: %d.\n", history.ActiveGrants, history.Unresolved, history.Capacity)
		if history.OverBudget {
			cmd.Println("Legacy state exceeds the budget. Existing grants and revocations are preserved; new grants require recovery or explicit historical abandonment.")
		}
		for _, item := range history.Revocations {
			cmd.Printf("%s\t%q\t%s\t%s\n  principal %s\n", item.OperationID, item.Name, item.Resolution.State, item.RevokedAt.UTC().Format(time.RFC3339), item.Principal)
			for _, daemon := range item.Resolution.Daemons {
				state := "unacknowledged"
				if daemon.Acknowledged {
					state = "acknowledged"
				}
				cmd.Printf("  daemon %s: %s\n", daemon.InstanceID, state)
			}
		}
		return nil
	},
}

var connectionsAbandonRevocationCmd = &cobra.Command{
	Use:   "abandon-revocation <name> <operation-id>",
	Short: "Abandon an exact historical drain acknowledgement without changing grants",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		confirmed, err := cmd.Flags().GetBool("confirm-unacknowledged")
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("this stops retaining an unresolved historical acknowledgement and does not prove its work stopped; use --confirm-unacknowledged only after deciding to abandon that exact outcome")
		}
		if err := connection.AbandonRevocationAcknowledgement(cmd.Context(), args[0], args[1]); err != nil {
			return err
		}
		cmd.Println("Historical acknowledgement is resolved or explicitly abandoned. No active grant was changed and no additional live drain is asserted.")
		return nil
	},
}

var connectionsAuditPruneCmd = &cobra.Command{
	Use:   "audit-prune",
	Short: "Remove completed or abandoned history while preserving active and unresolved state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := connection.PruneAuthorizationHistory(cmd.Context()); err != nil {
			return err
		}
		cmd.Println("Pruned completed and explicitly abandoned history. Active grants and unresolved receipts remain.")
		return nil
	},
}

func init() {
	connectionsAbandonRevocationCmd.Flags().Bool("confirm-unacknowledged", false, "Explicitly abandon the unknown historical drain result")
	connectionsCmd.AddCommand(connectionsRevocationsCmd, connectionsAbandonRevocationCmd, connectionsAuditPruneCmd)
}
