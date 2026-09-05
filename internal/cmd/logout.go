package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/spf13/cobra"
)

var (
	logoutHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	logoutItemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	logoutPromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
)

var logoutCmd = &cobra.Command{
	Aliases: []string{"signout"},
	Use:     "logout [platform]",
	Short:   "Logout Crux from a platform",
	Long: `Logout Crux from a registered OAuth platform, removing its provider
credential and stored accounts. With no argument, choose a logged-in platform.`,
	Example: `
# Sign out from GitHub Copilot
crux logout copilot
  `,
	ValidArgs: oauthProviderCompletions(),
	Args:      cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		progressEnabled := ws.Config.Options.Progress == nil || *ws.Config.Options.Progress
		if progressEnabled && supportsProgressBar() {
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
			defer func() { _, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar) }()
		}

		var registration providerregistry.Registration
		if len(args) == 0 {
			registration, err = pickLoggedInProvider(client, ws.ID)
			if err != nil {
				return err
			}
			if registration.ProviderID == "" {
				return nil
			}
		} else {
			var ok bool
			registration, ok = ws.Config.ProviderRegistrationForAccount(args[0])
			if !ok || registration.OAuth == nil {
				return fmt.Errorf("unknown OAuth platform: %s", args[0])
			}
		}

		force, _ := cmd.Flags().GetBool("force")
		if !force {
			fmt.Print(logoutPromptStyle.Render(fmt.Sprintf("Are you sure you want to logout %s? (y/N) ", registration.Name)))
			var response string
			_, err := fmt.Scanln(&response)
			if err != nil || (response != "y" && response != "Y" && response != "yes" && response != "Yes" && response != "YES") {
				fmt.Println(logoutHeaderStyle.Render("Logout cancelled."))
				return nil
			}
		}
		return logoutProvider(client, ws.ID, registration)
	},
}

func logoutProvider(client *client.Client, workspaceID string, registration providerregistry.Registration) error {
	ctx := getLogoutContext()
	owner := registration.Owner()
	validate := func() error {
		cfg, err := client.GetConfig(ctx, workspaceID)
		if err != nil {
			return err
		}
		current, ok := cfg.ProviderOwner(owner.ProviderID)
		if !ok || current != owner {
			return fmt.Errorf("provider account owner %s changed", owner.ProviderID)
		}
		return nil
	}
	if err := validate(); err != nil {
		return err
	}
	if err := client.RemoveProviderCredentials(ctx, workspaceID, config.ScopeGlobal, owner); err != nil {
		return err
	}
	if registration.AccountNamespace != "" {
		if err := validate(); err != nil {
			return err
		}
		if err := accounts.RemoveProviderForOwner(ctx, registration.AccountNamespace, validate); err != nil {
			return err
		}
	}
	fmt.Println(logoutHeaderStyle.Render("Successfully logged out of " + registration.Name + "."))
	return nil
}

func pickLoggedInProvider(client *client.Client, workspaceID string) (providerregistry.Registration, error) {
	ctx := getLogoutContext()
	cfg, err := client.GetConfig(ctx, workspaceID)
	if err != nil {
		return providerregistry.Registration{}, fmt.Errorf("failed to get config: %w", err)
	}

	var loggedIn []providerregistry.Registration
	for _, registration := range oauthRegistrations(cfg) {
		if provider, ok := cfg.Providers.Get(registration.ProviderID); ok && provider.OAuthToken != nil {
			loggedIn = append(loggedIn, registration)
		}
	}
	if len(loggedIn) == 0 {
		fmt.Println(logoutPromptStyle.Render("You are not logged in to any platform."))
		return providerregistry.Registration{}, nil
	}
	if len(loggedIn) == 1 {
		return loggedIn[0], nil
	}

	fmt.Println(logoutHeaderStyle.Render("Logged-in platforms:"))
	for i, registration := range loggedIn {
		fmt.Println(logoutItemStyle.Render(fmt.Sprintf("  %d. %s", i+1, registration.Name)))
	}
	fmt.Print(logoutPromptStyle.Render(fmt.Sprintf("Select a platform to logout (1-%d): ", len(loggedIn))))
	var choice int
	_, err = fmt.Scanln(&choice)
	if err != nil || choice < 1 || choice > len(loggedIn) {
		fmt.Println(logoutHeaderStyle.Render("Logout cancelled."))
		return providerregistry.Registration{}, nil
	}
	return loggedIn[choice-1], nil
}

func init() {
	logoutCmd.Flags().BoolP("force", "f", false, "Skip logout confirmation prompt")
}

func getLogoutContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
		os.Exit(1)
	}()
	return ctx
}
