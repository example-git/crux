package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/clipboard"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/workspace"
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
	ValidArgs: oauthProviderCompletions(),
	Args:      cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		registration, err := resolveOAuthRegistration(args, ws.Config())
		if err != nil {
			return err
		}
		force, _ := cmd.Flags().GetBool("force")
		return loginProvider(ws, registration, force)
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

func resolveOAuthRegistration(args []string, cfg *config.Config) (providerregistry.Registration, error) {
	if len(args) == 0 {
		registrations := oauthRegistrations(cfg)
		if len(registrations) > 0 {
			return registrations[0], nil
		}
		return providerregistry.Registration{}, fmt.Errorf("no OAuth provider is registered")
	}
	registration, ok := cfg.ProviderRegistrationForAccount(args[0])
	if !ok || registration.OAuth == nil {
		return providerregistry.Registration{}, fmt.Errorf("unknown OAuth platform: %s", args[0])
	}
	return registration, nil
}

func validateLoginOwner(ws workspace.Workspace, owner providerregistry.RegistrationOwner) error {
	current, ok := ws.Config().ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return fmt.Errorf("provider account owner %s changed", owner.ProviderID)
	}
	return nil
}

func loginProvider(ws workspace.Workspace, registration providerregistry.Registration, force bool) error {
	ctx := getLoginContext()
	if !force {
		if cfg := ws.Config(); cfg != nil {
			if provider, ok := cfg.Providers.Get(registration.ProviderID); ok && provider.OAuthToken != nil {
				fmt.Printf("You are already logged in to %s.\n", registration.Name)
				fmt.Println("Use --force to re-authenticate.")
				return nil
			}
		}
	}

	owner := registration.Owner()
	validate := func() error { return validateLoginOwner(ws, owner) }
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := validate(); err != nil {
		return err
	}
	token, err := authorizeProvider(ctx, registration)
	if err != nil {
		return err
	}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	accountID, displayName, raw := providerAccountIdentity(ctx, registration, token)
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	credential := config.ProviderOAuthCredential{Owner: owner, Token: token}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	if err := ws.SetProviderAPIKey(config.ScopeGlobal, registration.ProviderID, credential); err != nil {
		return err
	}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}
	if err := saveOAuthAccount(ctx, registration, accountID, displayName, raw, token, func() error {
		return validateLoginOwner(ws, owner)
	}); err != nil {
		return err
	}
	if err := validateLoginOwner(ws, owner); err != nil {
		return err
	}

	fmt.Println()
	if displayName != "default" {
		fmt.Printf("You're now authenticated with %s as %s!\n", registration.Name, displayName)
	} else {
		fmt.Printf("You're now authenticated with %s!\n", registration.Name)
	}
	return nil
}

func authorizeProvider(ctx context.Context, registration providerregistry.Registration) (*oauth.Token, error) {
	capability := registration.OAuth
	if capability == nil {
		return nil, fmt.Errorf("provider %s has no OAuth capability", registration.ProviderID)
	}
	if capability.Import != nil {
		token, found, err := capability.Import(ctx)
		if err != nil {
			return nil, fmt.Errorf("import %s credential: %w", registration.Name, err)
		}
		if found {
			fmt.Printf("Found an existing %s credential on disk.\n", registration.Name)
			return token, nil
		}
	}

	switch capability.Adapter {
	case providerregistry.LoginBrowser, providerregistry.LoginHostedPaste:
		if capability.Authorize == nil {
			return nil, fmt.Errorf("OAuth login for provider %s is declared but its core interpreter is unavailable", registration.ProviderID)
		}
		fmt.Printf("Opening browser for %s authorization...\n", registration.Name)
		open := func(url string) error {
			if err := providertransport.ValidateContextOwner(ctx); err != nil {
				return err
			}
			fmt.Println()
			fmt.Println("If the browser doesn't open, visit:")
			fmt.Println()
			lipgloss.Println(lipgloss.NewStyle().Hyperlink(url, "id="+registration.ProviderID).Render(url))
			fmt.Println()
			return providertransport.OpenURLWithContextOwnerValidator(ctx, browser.OpenURL, url)
		}
		var read providerregistry.ReadCode
		if capability.Adapter == providerregistry.LoginHostedPaste {
			read = func() (string, error) {
				fmt.Println("After approving access, paste the authorization code")
				fmt.Println("(or the full callback URL) here and press enter:")
				fmt.Print("> ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && line == "" {
					return "", err
				}
				return strings.TrimSpace(line), nil
			}
		}
		return capability.Authorize(ctx, open, read)
	case providerregistry.LoginDeviceCode:
		if capability.RequestDeviceCode == nil || capability.PollDeviceCode == nil {
			return nil, fmt.Errorf("device OAuth login for provider %s is unavailable", registration.ProviderID)
		}
		authorization, err := capability.RequestDeviceCode(ctx)
		if err != nil {
			return nil, err
		}
		clipboard.WriteText(authorization.UserCode)
		fmt.Println()
		fmt.Println("The following code should be on clipboard already:")
		fmt.Println()
		lipgloss.Println(lipgloss.NewStyle().Bold(true).Render(authorization.UserCode))
		fmt.Println()
		fmt.Println("Press enter to open this URL and authenticate:")
		fmt.Println()
		lipgloss.Println(lipgloss.NewStyle().Hyperlink(authorization.VerificationURL, "id="+registration.ProviderID).Render(authorization.VerificationURL))
		fmt.Println()
		waitEnter()
		if err := providertransport.OpenURLWithContextOwnerValidator(ctx, browser.OpenURL, authorization.VerificationURL); err != nil {
			if ownerErr := providertransport.ValidateContextOwner(ctx); ownerErr != nil {
				return nil, ownerErr
			}
			fmt.Println("Could not open the URL. You'll need to manually open it in your browser.")
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return nil, err
		}
		fmt.Println("Waiting for authorization...")
		return capability.PollDeviceCode(ctx, authorization)
	default:
		return nil, fmt.Errorf("provider %s uses unsupported OAuth adapter %q", registration.ProviderID, capability.Adapter)
	}
}

func providerAccountIdentity(ctx context.Context, registration providerregistry.Registration, token *oauth.Token) (string, string, []byte) {
	accountID, displayName := "", ""
	var raw []byte
	if registration.Identity != nil && token != nil {
		accountID, displayName, raw = registration.Identity(ctx, token.AccessToken)
	}
	if accountID == "" {
		accountID = "default"
	}
	if displayName == "" {
		displayName = accountID
	}
	return accountID, displayName, raw
}

// saveOAuthAccount stores the credential in the multi-account store. Provider
// configuration remains the inference credential source during migration.
func saveOAuthAccount(ctx context.Context, registration providerregistry.Registration, accountID, displayName string, raw []byte, token *oauth.Token, validate accounts.Validator) error {
	if registration.AccountNamespace == "" || token == nil {
		return nil
	}
	entry := accounts.FromToken(accountID, displayName, token, nil)
	entry.Raw = raw
	return accounts.SaveForOwner(ctx, registration.AccountNamespace, entry, validate)
}

func getLoginContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
		os.Exit(1)
	}()
	return ctx
}

func waitEnter() {
	_, _ = fmt.Scanln()
}
