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
func providerArg(name string) (providerregistry.Registration, bool) {
	registry := config.ProviderCapabilities()
	if registration, ok := registry.Lookup(name); ok && registration.AccountNamespace != "" {
		return registration, true
	}
	providerID := accounts.ProviderID(name)
	registration, ok := registry.Lookup(providerID)
	return registration, ok && registration.AccountNamespace != ""
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
	providers, err := accounts.Providers(ctx)
	if err != nil {
		return err
	}
	registry := config.ProviderCapabilities()
	activeProviders := providers[:0]
	for _, provider := range providers {
		if registry.HasAccountNamespace(provider) {
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
		registration, ok := providerArg(args[0])
		if !ok {
			return fmt.Errorf("unknown provider: %s", args[0])
		}
		ctx := cmd.Context()
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := accounts.SetActive(ctx, registration.AccountNamespace, args[1]); err != nil {
			return err
		}
		entry, err := accounts.Active(ctx, registration.AccountNamespace)
		if err != nil {
			return fmt.Errorf("load active account after switch: %w", err)
		}
		if entry == nil {
			return fmt.Errorf("active account unavailable after switch")
		}
		fresh, err := accounts.EnsureFresh(ctx, registration.AccountNamespace, entry)
		if err != nil {
			return err
		}
		if err := ws.SetProviderAPIKey(config.ScopeGlobal, registration.ProviderID, fresh.Token()); err != nil {
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
		registration, ok := providerArg(args[0])
		if !ok {
			return fmt.Errorf("unknown provider: %s", args[0])
		}
		if err := accounts.Remove(cmd.Context(), registration.AccountNamespace, args[1]); err != nil {
			return err
		}
		fmt.Printf("Removed %s account %s.\n", registration.Name, args[1])
		return nil
	},
}

func init() {
	accountsCmd.AddCommand(accountsListCmd, accountsSwitchCmd, accountsRemoveCmd)
}
