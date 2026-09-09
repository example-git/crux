package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/spf13/cobra"
)

// providerArg resolves a provider ID, alias, or registered account namespace.
func providerArg(cfg *config.Config, name string) (providerregistry.Registration, bool) {
	return cfg.ProviderRegistrationForAccount(name)
}

var accountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage stored OAuth accounts",
	Long: `List, switch, and remove stored OAuth accounts.
Available providers are determined by the registered OAuth capabilities.
The active account per provider is the one used for requests.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAccountsList(cmd)
	},
}

var accountsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored accounts for all providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAccountsList(cmd)
	},
}

func runAccountsList(cmd *cobra.Command) error {
	ctx := cmd.Context()
	ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	return listWorkspaceAccounts(ctx, ws, cmd.OutOrStdout())
}

var accountsSwitchCmd = &cobra.Command{
	Use:   "switch <provider> <account-id>",
	Short: "Set the active account for a provider",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		return switchWorkspaceAccount(ctx, ws, args[0], args[1], cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

var accountsRemoveCmd = &cobra.Command{
	Use:   "remove <provider> <account-id>",
	Short: "Remove a stored account",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		registration, ok := providerArg(ws.Config(), args[0])
		if !ok {
			return fmt.Errorf("unknown provider: %s", args[0])
		}
		owner := registration.Owner()
		validate := func() error {
			current, active := ws.Config().ProviderOwner(registration.ProviderID)
			if !active || current != owner {
				return fmt.Errorf("provider account owner %s changed before account removal", args[0])
			}
			return nil
		}
		if err := accounts.RemoveForOwner(cmd.Context(), registration.AccountNamespace, args[1], validate); err != nil {
			return err
		}
		fmt.Printf("Removed %s account %s.\n", registration.Name, args[1])
		return nil
	},
}

var accountsLogoutCmd = &cobra.Command{
	Use: "logout <provider>", Short: "Clear a provider credential and all its saved accounts", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		return logoutWorkspaceProvider(ctx, ws, args[0], cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

func init() {
	accountsCmd.AddCommand(accountsListCmd, accountsSwitchCmd, accountsRemoveCmd, accountsLogoutCmd)
}
