package cmd

import (
	"os"
	"os/signal"

	"github.com/example-git/crux/internal/clipboard"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Aliases: []string{"auth"},
	Use:     "login [platform]",
	Short:   "Login Crux to a platform",
	Long: `Login Crux to a specified platform.
The platform must expose a registered OAuth capability.`,
	Example: `
# Authenticate with the first registered OAuth provider
crux login

# Authenticate with GitHub Copilot
crux login copilot

# Force re-authentication even if already logged in
crux login --force copilot
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		force, _ := cmd.Flags().GetBool("force")
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		return runWorkspaceLogin(ctx, ws, args, force, cmd.InOrStdin(), cmd.OutOrStdout(), browser.OpenURL, func(code string) { clipboard.WriteText(code) })
	},
}

func init() {
	loginCmd.Flags().BoolP("force", "f", false, "Force re-authentication even if already logged in")
}

func oauthRegistrations(cfg *config.Config) []providerregistry.Registration {
	registrations := config.ProviderCapabilities().Registrations()
	if cfg != nil {
		registrations = cfg.ProviderRegistrations()
	}
	result := registrations[:0]
	for _, registration := range registrations {
		if registration.OAuth != nil {
			result = append(result, registration)
		}
	}
	return result
}

func oauthProviderCompletions() []cobra.Completion {
	var result []cobra.Completion
	for _, registration := range oauthRegistrations(nil) {
		result = append(result, cobra.Completion(registration.ProviderID))
		for _, alias := range registration.Aliases {
			result = append(result, cobra.Completion(alias))
		}
	}
	return result
}
