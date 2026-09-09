package cmd

import (
	"errors"
	"os"
	"os/signal"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/workspace"
	"github.com/spf13/cobra"
)

var authenticationAbandonCmd = &cobra.Command{
	Use:   "abandon-publication <original-workspace-id> <operation-or-review-id>",
	Short: "Retire recovery of an exact original authentication publication",
	Long:  "First inspect accounts history. Supply its nonzero journal revision and a distinct abandonment action ID; reuse the same command to retry that action. Add --review only when the second argument names an attempted review. Retirement preserves original progress and any unknown receiver outcome. It performs no account repair, exchange, or runtime publication.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		revision, err := cmd.Flags().GetUint64("revision")
		if err != nil {
			return err
		}
		abandonID, err := cmd.Flags().GetString("abandon-id")
		if err != nil {
			return err
		}
		isReview, err := cmd.Flags().GetBool("review")
		if err != nil {
			return err
		}
		if revision == 0 || abandonID == "" || abandonID == args[1] {
			return errors.New("--revision requires the reviewed nonzero journal revision and --abandon-id requires a distinct action ID")
		}
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()
		historian, ok := ws.(workspace.ProviderAuthenticationHistorian)
		if !ok {
			return errors.New("publication retirement requires the owning client workspace")
		}
		currentID := ws.AuthenticationWorkspaceID()
		history, err := historian.ProviderAuthenticationHistory(ctx)
		if err != nil {
			return err
		}
		if currentID == "" || currentID != ws.AuthenticationWorkspaceID() {
			return providerauth.ErrStale
		}
		if isReview {
			capability, ok := ws.(workspace.ProviderAuthenticationReviewAbandoner)
			if !ok {
				return errors.New("review publication retirement is unavailable")
			}
			for _, entry := range history.Reviews {
				target := entry.Request.OriginalTarget
				if entry.Request.FreshSaved {
					target = entry.Request.SavedTarget
				}
				if target.WorkspaceID != args[0] || entry.Request.ReviewID != args[1] {
					continue
				}
				request := workspace.ProviderAuthenticationReviewAbandonRequest{WorkspaceID: currentID, Review: entry.Request, PreviewID: entry.Summary.PreviewID, Revision: revision, AbandonID: abandonID}
				if err := request.Validate(); err != nil {
					return err
				}
				outcome, err := capability.AbandonProviderAuthenticationReview(ctx, request)
				if outcome.Validate(request) == nil && entry.ApplyRequest != nil && outcome.Original.Validate(*entry.ApplyRequest) == nil {
					cmd.Printf("Review %q in original workspace %q was retired by %q. Original receiver acknowledged=%t; adopted=%t.\n", args[1], args[0], abandonID, outcome.Original.RemoteAcknowledged, outcome.Original.Adopted)
					cmd.Println("The original apply outcome is unchanged. This action made no receiver publication and did not undo any previous one.")
				} else if err == nil {
					return providerauth.ErrReceiptUnverified
				}
				return err
			}
			return errors.New("exact original review is unavailable in this owning client's retained history")
		}
		capability, ok := ws.(workspace.ProviderAuthenticationAbandoner)
		if !ok {
			return errors.New("original publication retirement is unavailable")
		}
		for _, entry := range history.Operations {
			if entry.Target.WorkspaceID != args[0] || entry.OperationID != args[1] {
				continue
			}
			request := workspace.ProviderAuthenticationAbandonRequest{WorkspaceID: currentID, Target: entry.Target, OperationID: entry.OperationID, Revision: revision, AbandonID: abandonID}
			if err := request.Validate(); err != nil {
				return err
			}
			outcome, err := capability.AbandonProviderAuthentication(ctx, request)
			originalIdentityMatches := outcome.Original.CheckID == entry.Outcome.CheckID && outcome.Original.CredentialID == entry.Outcome.CredentialID && outcome.Original.LoginID == entry.Outcome.LoginID && outcome.Original.RemovedAccountID == entry.Outcome.RemovedAccountID
			if outcome.Validate(request) == nil && originalIdentityMatches {
				p := outcome.Original.Progress
				cmd.Printf("Operation %q in original workspace %q was retired by %q. Original receiver acknowledged=%t; adopted=%t.\n", args[1], args[0], abandonID, outcome.RemoteAcknowledged, outcome.Adopted)
				cmd.Printf("Original observed progress: refreshed=%t accounts=%t configuration=%t local runtime=%t.\n", p.AccountRefreshed, p.AccountsSaved, p.ConfigSaved, p.RuntimePublished)
				cmd.Println("The original outcome is unchanged. This action performed no exchange, disk repair, or receiver publication.")
			} else if err == nil {
				return providerauth.ErrReceiptUnverified
			}
			return err
		}
		return errors.New("exact original operation is unavailable in this owning client's retained history")
	},
}

func init() {
	authenticationAbandonCmd.Flags().Uint64("revision", 0, "Exact journal revision from the reviewed original history record")
	authenticationAbandonCmd.Flags().String("abandon-id", "", "Distinct 32-character lowercase hexadecimal action ID; reuse the same ID and revision for an exact retry")
	authenticationAbandonCmd.Flags().Bool("review", false, "Select a retained attempted review instead of an original operation")
	_ = authenticationAbandonCmd.MarkFlagRequired("revision")
	_ = authenticationAbandonCmd.MarkFlagRequired("abandon-id")
	accountsCmd.AddCommand(authenticationAbandonCmd)
}
