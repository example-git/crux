package cmd

import (
	"fmt"
	"time"

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
	cfg := ws.Config()
	providers, err := accounts.ProvidersFor(ctx, cfg.ProviderAccountNamespaces())
	if err != nil {
		return err
	}
	activeProviders := providers[:0]
	for _, provider := range providers {
		if _, ok := cfg.ProviderRegistrationForAccount(provider); ok {
			activeProviders = append(activeProviders, provider)
		}
	}
	if len(activeProviders) == 0 {
		fmt.Println("No stored accounts for active providers. Use `crux login <provider>` to add one.")
		return nil
	}
	for _, provider := range activeProviders {
		list, err := accounts.List(ctx, provider)
		if err != nil {
			return err
		}
		active, err := accounts.Active(ctx, provider)
		if err != nil {
			return err
		}
		fmt.Printf("%s:\n", provider)
		for _, entry := range list {
			marker := " "
			if active != nil && entry.ID == active.ID {
				marker = "*"
			}
			status := ""
			if entry.Expired() {
				if entry.RefreshToken != "" {
					status = " (expired, refreshable)"
				} else {
					status = " (expired)"
				}
			} else if entry.ExpiresAt > 0 {
				status = fmt.Sprintf(" (expires %s)",
					time.UnixMilli(entry.ExpiresAt).Format(time.RFC3339))
			}
			fmt.Printf("  %s %s%s\n", marker, entry.DisplayName, status)
		}
	}
	return nil
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
		registration, ok := providerArg(ws.Config(), args[0])
		if !ok {
			return fmt.Errorf("unknown provider: %s", args[0])
		}
		entries, err := accounts.List(ctx, registration.AccountNamespace)
		if err != nil {
			return err
		}
		var entry *accounts.Entry
		for index := range entries {
			if entries[index].ID == args[1] {
				entry = &entries[index]
				break
			}
		}
		if entry == nil {
			return fmt.Errorf("account %q not found for provider %q", args[1], registration.AccountNamespace)
		}
		owner := registration.Owner()
		validate := func() error {
			current, ok := ws.Config().ProviderOwner(registration.ProviderID)
			if !ok || current != owner {
				return fmt.Errorf("provider account owner %s changed", args[0])
			}
			return nil
		}
		if err := validate(); err != nil {
			return err
		}
		var refresher accounts.Refresher
		if registration.OAuth != nil {
			refresher = registration.OAuth.Refresh
		}
		fresh, err := accounts.EnsureFreshForOwner(ctx, registration.AccountNamespace, entry, refresher, validate)
		if err != nil {
			return err
		}
		if err := validate(); err != nil {
			return err
		}
		credential := config.ProviderOAuthCredential{Owner: owner, Token: fresh.Token()}
		if err := ws.SetProviderAPIKey(config.ScopeGlobal, registration.ProviderID, credential); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return err
		}
		if err := accounts.SetActiveForOwner(ctx, registration.AccountNamespace, args[1], validate); err != nil {
			return err
		}
		fmt.Printf("Active %s account is now %s.\n", registration.Name, args[1])
		return nil
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

func init() {
	accountsCmd.AddCommand(accountsListCmd, accountsSwitchCmd, accountsRemoveCmd)
}
