package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/spf13/cobra"
)

var authenticationLocalRepairCmd = &cobra.Command{
	Use:   "repair-local <original-workspace-id> <operation-id>",
	Short: "Review, repair, or abandon a retained authentication disk operation",
	Long:  "Review the original account/config write progress after interruption or restart. Add --apply-revision with the reviewed revision to finish only its exact staged disk writes. Use --abandon-revision to stop retaining original recovery while preserving any unknown exchange outcome; it is separate from repair. Neither action repeats an exchange or publishes a runtime. After repair, explicitly reload and review saved authentication.",
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
		abandon, err := cmd.Flags().GetUint64("abandon-revision")
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("abandon-revision") && abandon == 0 {
			return fmt.Errorf("--abandon-revision requires the nonzero reviewed journal revision")
		}
		if cmd.Flags().Changed("apply-revision") && cmd.Flags().Changed("abandon-revision") {
			return fmt.Errorf("choose either --apply-revision or --abandon-revision")
		}
		applying := revision != 0
		if abandon != 0 {
			revision = abandon
		}
		request := providerauth.LocalRepairRequest{WorkspaceID: ws.AuthenticationWorkspaceID(), OperationWorkspaceID: args[0], OperationID: args[1], Revision: revision, Apply: applying, Abandon: abandon != 0}
		result, err := ws.RepairLocalAuthentication(ctx, request)
		response := proto.ProviderLocalRepairResponse{Request: request, Result: result}
		if err != nil {
			response.Error = proto.NewProviderLocalRepairError(err)
		}
		if validation := response.Validate(request); validation != nil {
			return validation
		}
		if result.Summary.OperationID != "" {
			s := result.Summary
			p := s.Original
			fmt.Fprintf(cmd.OutOrStdout(), "Original operation %s in %s: %s for provider %s, journal revision %d.\n", s.OperationID, s.WorkspaceID, s.Action, s.ProviderID, s.Revision)
			fmt.Fprintf(cmd.OutOrStdout(), "Original observed progress: refreshed=%t, accounts=%t, configuration=%t, local runtime=%t. Remote acknowledgement is not inferred.\n", p.AccountRefreshed, p.AccountsSaved, p.ConfigSaved, p.RuntimePublished)
			if s.RefreshStarted {
				fmt.Fprintf(cmd.OutOrStdout(), "Refresh exchange started; returned token retained=%t.\n", s.RefreshObserved)
			}
			if s.RepairStarted {
				fmt.Fprintf(cmd.OutOrStdout(), "Prior repair observed writes: accounts=%t, configuration=%t. Abandonment does not undo them.\n", s.RepairAccountsWritten, s.RepairConfigWritten)
			}
			if s.Abandoned {
				fmt.Fprintln(cmd.OutOrStdout(), "Original local recovery was explicitly abandoned. Its observed progress and any unknown exchange outcome remain unchanged; this abandonment performed no file repair or runtime publication.")
			} else if s.NoEffects {
				fmt.Fprintln(cmd.OutOrStdout(), "The local save attempt finished without starting an account refresh or staging an account/configuration write. Its capacity reservation is released; no saved state or runtime is asserted.")
			}
			if request.Apply {
				fmt.Fprintf(cmd.OutOrStdout(), "Repair progress: accounts written=%t, accounts matched=%t, configuration written=%t, configuration matched=%t.\n", result.AccountsWritten, result.AccountsMatched, result.ConfigWritten, result.ConfigMatched)
			} else if !request.Abandon && !s.Abandoned && !s.NoEffects && !s.Coherent && !s.NeedsReload {
				if s.RepairReady && (!s.RefreshStarted || s.RefreshObserved) {
					fmt.Fprintf(cmd.OutOrStdout(), "To apply this reviewed fixed write, repeat with --apply-revision %d.\n", s.Revision)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "To explicitly abandon original local recovery while preserving its outcome, repeat with --abandon-revision %d.\n", s.Revision)
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
	authenticationLocalRepairCmd.Flags().Uint64("abandon-revision", 0, "Abandon original recovery at the exact reviewed revision; keep unknown outcome separate from repair")
	accountsCmd.AddCommand(authenticationLocalRepairCmd)
}
