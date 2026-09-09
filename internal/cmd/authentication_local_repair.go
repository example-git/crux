package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/spf13/cobra"
)

var authenticationLocalRepairCmd = &cobra.Command{
	Use:   "repair-local <original-workspace-id> <operation-id>",
	Short: "Review or finish a retained authentication disk operation",
	Long:  "Review the original account/config write progress after interruption or restart. Add --apply-revision with the reviewed revision to finish only its exact staged disk writes. Repair never repeats a token exchange or publishes a runtime; afterward explicitly reload and review saved authentication.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		revision, err := cmd.Flags().GetUint64("apply-revision")
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("apply-revision") && revision == 0 {
			return fmt.Errorf("--apply-revision requires the nonzero reviewed journal revision")
		}
		request := providerauth.LocalRepairRequest{WorkspaceID: ws.AuthenticationWorkspaceID(), OperationWorkspaceID: args[0], OperationID: args[1], Revision: revision, Apply: revision != 0}
		result, err := ws.RepairLocalAuthentication(ctx, request)
		if result.Summary.OperationID != "" {
			s := result.Summary
			p := s.Original
			fmt.Fprintf(cmd.OutOrStdout(), "Original operation %s in %s: %s for provider %s, journal revision %d.\n", s.OperationID, s.WorkspaceID, s.Action, s.ProviderID, s.Revision)
			fmt.Fprintf(cmd.OutOrStdout(), "Original observed progress: refreshed=%t, accounts=%t, configuration=%t, local runtime=%t. Remote acknowledgement is not inferred.\n", p.AccountRefreshed, p.AccountsSaved, p.ConfigSaved, p.RuntimePublished)
			if s.RefreshStarted {
				fmt.Fprintf(cmd.OutOrStdout(), "Refresh exchange started; returned token retained=%t.\n", s.RefreshObserved)
			}
			if request.Apply {
				fmt.Fprintf(cmd.OutOrStdout(), "Repair progress: accounts written=%t, accounts matched=%t, configuration written=%t, configuration matched=%t.\n", result.AccountsWritten, result.AccountsMatched, result.ConfigWritten, result.ConfigMatched)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "To apply this reviewed revision, repeat this command with --apply-revision %d.\n", s.Revision)
			}
			if result.NeedsReload || s.NeedsReload {
				if s.Coherent {
					fmt.Fprintln(cmd.OutOrStdout(), "The original local transaction completed coherently. No additional disk repair is performed.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Disk repair progress is retained separately from the original transaction.")
				}
				fmt.Fprintln(cmd.OutOrStdout(), "In the owning client, open Review saved authentication, explicitly Reload, then review and apply the exact saved choice. This is a new runtime change, separate from the original operation.")
			}
		}
		return err
	},
}

func init() {
	authenticationLocalRepairCmd.Flags().Uint64("apply-revision", 0, "Apply the exact journal revision previously reviewed (omit to review only)")
	accountsCmd.AddCommand(authenticationLocalRepairCmd)
}
