package cmd

import (
	"github.com/example-git/crux/internal/server"
	"github.com/spf13/pflag"
)

func initializeProviderCompletions() {
	providers := oauthProviderCompletions()
	loginCmd.ValidArgs = providers
	logoutCmd.ValidArgs = providers
}

// InitializeEnvironmentDefaults runs after main loads dotenv for ordinary
// commands. Completion skips dotenv and uses only the process environment.
func InitializeEnvironmentDefaults() {
	initializeProviderCompletions()
	for _, flags := range []*pflag.FlagSet{rootCmd.PersistentFlags(), serverCmd.Flags()} {
		if flag := flags.Lookup("host"); flag != nil && !flag.Changed {
			value := server.DefaultHost()
			_ = flag.Value.Set(value)
			flag.DefValue = value
		}
	}
}
