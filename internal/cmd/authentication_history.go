package cmd

import (
	"encoding/json"
	"errors"

	"github.com/example-git/crux/internal/workspace"
	"github.com/spf13/cobra"
)

var authenticationHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Inspect retained owning-client authentication operations and reviews",
	Long:  "List redacted historical local progress and receiver publication receipts. Reading history does not repeat a save, exchange, recovery or publication. Use the authentication history UI for explicit recovery or review, or accounts repair-local with the original workspace and operation IDs for a retained partial disk transaction.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		historian, ok := ws.(workspace.ProviderAuthenticationHistorian)
		if !ok {
			return errors.New("authentication publication history requires an owning-client workspace")
		}
		history, err := historian.ProviderAuthenticationHistory(cmd.Context())
		if err != nil {
			return err
		}
		asJSON, err := cmd.Flags().GetBool("json")
		if err != nil {
			return err
		}
		if asJSON {
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(history)
		}
		cmd.Println("Historical receipts; current credentials and receiver state are not inferred.")
		if len(history.Operations) == 0 && len(history.Reviews) == 0 {
			cmd.Println("No retained authentication publication history for this connection and workspace scope.")
			return nil
		}
		for _, operation := range history.Operations {
			progress := operation.Outcome.Progress
			cmd.Printf("Operation %q: provider %q; original workspace %q.\n", operation.OperationID, operation.Target.Owner.ProviderID, operation.Target.WorkspaceID)
			if operation.HistoricalWorkspace {
				cmd.Println("  Earlier workspace incarnation: read-only history; its target cannot publish into this workspace.")
			}
			cmd.Printf("  Local observed: refreshed=%t accounts=%t configuration=%t runtime=%t; local result retained=%t.\n", progress.AccountRefreshed, progress.AccountsSaved, progress.ConfigSaved, progress.RuntimePublished, operation.LocalFinished)
			cmd.Printf("  Receiver acknowledged=%t; adopted=%t; recovery sequence=%d; review sequence=%d.\n", operation.RemoteAcknowledged, operation.Adopted, operation.RecoverySequence, operation.ReviewSequence)
			cmd.Printf("  Journal revision=%d; publication recovery abandoned=%t.\n", operation.JournalRevision, operation.Abandoned)
			if operation.AbandonRequest != nil {
				cmd.Printf("  Retained abandonment action %q from revision %d; original outcome unchanged.\n", operation.AbandonRequest.AbandonID, operation.AbandonRequest.Revision)
			}
			if operation.RecoveryRequest != nil {
				cmd.Printf("  Retained recovery request %q (sequence %d).\n", operation.RecoveryRequest.RecoveryID, operation.RecoveryRequest.RecoverySequence)
			}
			if operation.ReconciledBy != "" {
				cmd.Printf("  Separately reconciled by preview %q.\n", operation.ReconciledBy)
			}
			if operation.SavedStateSupersededBy != "" {
				cmd.Printf("  Publication superseded by fresh saved-state preview %q; original result unchanged.\n", operation.SavedStateSupersededBy)
			}
		}
		for _, review := range history.Reviews {
			cmd.Printf("Review %q: preview %q; original operation %q; fresh saved choice=%t.\n", review.Request.ReviewID, review.Summary.PreviewID, review.Request.OperationID, review.Request.FreshSaved)
			originalTarget := review.Request.OriginalTarget
			if review.Request.FreshSaved {
				originalTarget = review.Request.SavedTarget
			}
			cmd.Printf("  Original workspace %q; journal revision=%d; publication recovery abandoned=%t.\n", originalTarget.WorkspaceID, review.JournalRevision, review.Abandoned)
			if review.AbandonRequest != nil {
				cmd.Printf("  Retained review abandonment action %q from revision %d; original apply outcome unchanged.\n", review.AbandonRequest.AbandonID, review.AbandonRequest.Revision)
			}
			if review.HistoricalWorkspace {
				cmd.Println("  Earlier workspace incarnation: this preview cannot be applied to the current workspace.")
			}
			if review.ApplyRequest != nil {
				cmd.Printf("  Apply request %q.\n", review.ApplyRequest.ApplyID)
			}
			if review.ApplyOutcome != nil {
				cmd.Printf("  Receiver acknowledged=%t; adopted=%t; original disposition=%q.\n", review.ApplyOutcome.RemoteAcknowledged, review.ApplyOutcome.Adopted, review.ApplyOutcome.OriginalDisposition)
			}
			if review.SavedStateSupersededBy != "" {
				cmd.Printf("  Superseded by fresh saved-state preview %q.\n", review.SavedStateSupersededBy)
			}
			if review.SupersededByReview != "" {
				cmd.Printf("  Unused preview superseded by review %q.\n", review.SupersededByReview)
			}
			if review.OriginalAbandonedBy != "" {
				cmd.Printf("  Original publication recovery abandoned by action %q.\n", review.OriginalAbandonedBy)
			}
		}
		return nil
	},
}

func init() {
	authenticationHistoryCmd.Flags().Bool("json", false, "Print only the redacted public history metadata as JSON")
	accountsCmd.AddCommand(authenticationHistoryCmd)
}
