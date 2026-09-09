package cmd

import (
	"errors"
	"time"

	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
)

var connectionsPendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "List retained pairing identities awaiting recovery",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		items, err := connection.ListPendingPairings(cmd.Context())
		if err != nil {
			return err
		}
		if len(items) == 0 {
			cmd.Println("No pending pairings.")
			return nil
		}
		for _, item := range items {
			cmd.Printf("%s\t%s\t%s\t%s\n  client %s\n  server %s\n", item.OperationID, item.Name, item.Address, item.CreatedAt.Format(time.RFC3339), item.ClientFingerprint, item.ServerFingerprint)
			if item.PromotionName != "" {
				cmd.Printf("  intended saved name %s\n", item.PromotionName)
			}
		}
		return nil
	},
}

var connectionsRecoverCmd = &cobra.Command{
	Use:   "recover <operation-id> [alternate-local-name]",
	Short: "Recover the exact retained pairing identity after pinned server authorization",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		alias := ""
		if len(args) == 2 {
			alias = args[1]
		}
		saved, err := connection.RecoverPairing(cmd.Context(), args[0], alias)
		if err != nil {
			return err
		}
		cmd.Printf("Recovered connection %s with %s using its retained client identity.\n", saved.Name, saved.Address)
		return nil
	},
}

var connectionsForgetPendingCmd = &cobra.Command{
	Use:   "forget-pending <operation-id>",
	Short: "Delete a pending local key without revoking any server authorization",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		confirmed, err := cmd.Flags().GetBool("confirm-key-loss")
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("this deletes the retained local key and does not revoke a possible server grant; use --confirm-key-loss only after arranging server-side revocation or deciding to abandon this identity")
		}
		if err := connection.ForgetPendingPairing(cmd.Context(), args[0]); err != nil {
			return err
		}
		cmd.Println("Deleted pending local identity. Any server-side authorization is unchanged.")
		return nil
	},
}

func init() {
	connectionsForgetPendingCmd.Flags().Bool("confirm-key-loss", false, "Confirm deletion of this pending key without server-side revocation")
	connectionsCmd.AddCommand(connectionsPendingCmd, connectionsRecoverCmd, connectionsForgetPendingCmd)
}
