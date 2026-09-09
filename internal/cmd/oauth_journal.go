package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/spf13/cobra"
)

const oauthJournalScopeHelp = " This is a local journal-only recovery command: it does not load provider configuration, connect to a server, exchange credentials or publish a runtime. Supply the exact original absolute --global-config-data and --workspace-config paths, and --cwd for the original local working directory (defaults to the current directory). An explicitly empty --workspace-config is allowed only for a scope originally captured that way. The original provider may have been removed. Remote --connection/--host selectors and --data-dir are rejected; run on the owning machine with its original local paths. Records without a captured scope cannot be reassigned."

var pendingOAuthJournalCmd = &cobra.Command{
	Use:   "pending-oauth",
	Short: "List retained OAuth operation IDs and state in an explicit local scope",
	Long:  "List only original workspace IDs, operation IDs and recorded state. Reading does not retire an operation or infer credential save or receiver acknowledgement." + oauthJournalScopeHelp,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := oauthJournalCommandContext(cmd)
		defer stop()
		scope, err := openOAuthJournalCommandScope(ctx, cmd)
		if err != nil {
			return err
		}
		results, err := scope.Pending(ctx)
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
			return encoder.Encode(results)
		}
		if len(results) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "No retained OAuth operations for the exact supplied local scope.")
			return err
		}
		for _, result := range results {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Workspace %q, operation %q: %s; abandoned=%t.\n", result.OriginalWorkspaceID, result.OriginalOperationID, result.State, result.Abandoned); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Retire a selected tokenless operation with accounts retire-oauth and the same scope flags. A recorded token requires the separate --discard-recorded-token choice; it is never treated as an unknown result.")
		return err
	},
}

var retireOAuthJournalCmd = &cobra.Command{
	Use:   "retire-oauth <original-workspace-id> <original-operation-id>",
	Short: "Explicitly release one retained OAuth operation's recovery reservation",
	Long:  "Explicitly abandon an exact original tokenless preparation or unknown exchange. Its original outcome remains not-started or unknown. The operation lease waits for any active exchange and result write; if a token was recorded, retirement is refused unless --discard-recorded-token explicitly abandons recovery of that known result. The token and original evidence remain retained until bounded completed-history pruning. This does not revoke a provider token, cancel an already-authorized session, repeat an exchange, undo a save, or acknowledge a runtime." + oauthJournalScopeHelp,
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := oauthJournalCommandContext(cmd)
		defer stop()
		scope, err := openOAuthJournalCommandScope(ctx, cmd)
		if err != nil {
			return err
		}
		discard, err := cmd.Flags().GetBool("discard-recorded-token")
		if err != nil {
			return err
		}
		capture, err := scope.CaptureRetirement(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		result, err := capture.Retire(ctx, discard)
		if err != nil {
			return fmt.Errorf("OAuth retirement not verified; list the same scope or explicitly retry the same original IDs: %w", err)
		}
		if !result.Abandoned || result.OriginalWorkspaceID != args[0] || result.OriginalOperationID != args[1] {
			return errors.New("OAuth retirement returned an unmatched receipt")
		}
		if result.State == "token-result-recorded" {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Explicitly abandoned token recovery for workspace %q, operation %q. The original result remains token-result-recorded; it is not unknown. This operation's recorded token cannot be recovered again through Crux and is eligible for bounded pruning; already-authorized sessions are not canceled. No provider revocation, credential save or runtime acknowledgement is implied.\n", args[0], args[1])
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Retired workspace %q, operation %q; original state remains %s. Its reservation was released and evidence is eligible for bounded pruning. No exchange, credential save or runtime acknowledgement is implied.\n", args[0], args[1], result.State)
		}
		return err
	},
}

func oauthJournalCommandContext(cmd *cobra.Command) (context.Context, func()) {
	signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	ctx, cancel := context.WithTimeout(signalCtx, time.Minute)
	return ctx, func() { cancel(); stop() }
}

func openOAuthJournalCommandScope(ctx context.Context, cmd *cobra.Command) (config.OAuthLoginJournalScope, error) {
	if connectionName != "" || cmd.Flags().Changed("host") || cmd.Flags().Changed("connection") || cmd.Flags().Changed("data-dir") {
		return config.OAuthLoginJournalScope{}, errors.New("OAuth journal recovery is local-only; use explicit original config paths and local --cwd without --connection, --host or --data-dir")
	}
	if !cmd.Flags().Changed("global-config-data") || !cmd.Flags().Changed("workspace-config") {
		return config.OAuthLoginJournalScope{}, errors.New("supply both original --global-config-data and --workspace-config paths; an explicitly empty workspace path is accepted")
	}
	globalPath, err := cmd.Flags().GetString("global-config-data")
	if err != nil {
		return config.OAuthLoginJournalScope{}, err
	}
	workspacePath, err := cmd.Flags().GetString("workspace-config")
	if err != nil {
		return config.OAuthLoginJournalScope{}, err
	}
	cwd, err := cmd.Flags().GetString("cwd")
	if err != nil {
		return config.OAuthLoginJournalScope{}, err
	}
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return config.OAuthLoginJournalScope{}, err
		}
	}
	return config.OpenOAuthLoginJournalScope(ctx, globalPath, workspacePath, cwd)
}

func init() {
	for _, command := range []*cobra.Command{pendingOAuthJournalCmd, retireOAuthJournalCmd} {
		command.Flags().String("global-config-data", "", "Exact original absolute global writable config path (required)")
		command.Flags().String("workspace-config", "", "Exact original absolute workspace config path; explicitly empty only if originally captured empty (required)")
	}
	pendingOAuthJournalCmd.Flags().Bool("json", false, "Print only original operation IDs and recorded state as JSON")
	retireOAuthJournalCmd.Flags().Bool("discard-recorded-token", false, "Explicitly abandon recovery of the selected operation's observed token; preserve its known-result state until pruning")
	accountsCmd.AddCommand(pendingOAuthJournalCmd, retireOAuthJournalCmd)
}
