package cmd

import (
	"fmt"
	"log/slog"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/example-git/crux/internal/config"
	"github.com/spf13/cobra"
)

var updateProvidersSource string

var updateProvidersCmd = &cobra.Command{
	Use:   "update-providers [path-or-url]",
	Short: "Update providers",
	Long:  `Update provider information from a specified local path or remote URL.`,
	Example: `
# Reset providers to the embedded catalog
crux update-providers

# Update providers from an explicit URL
crux update-providers https://example.com/providers.json

# Update providers from a local file
crux update-providers /path/to/local-providers.json

# Reset providers to the embedded catalog explicitly
crux update-providers embedded
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// NOTE(@andreynering): We want to skip logging output do stdout here.
		slog.SetDefault(slog.New(slog.DiscardHandler))

		var pathOrURL string
		if len(args) > 0 {
			pathOrURL = args[0]
		}

		var err error
		switch updateProvidersSource {
		case "catwalk":
			err = config.UpdateProviders(pathOrURL)
		default:
			return fmt.Errorf("invalid source %q, must be 'catwalk'", updateProvidersSource)
		}

		if err != nil {
			return err
		}

		// NOTE(@andreynering): This style is more-or-less copied from Fang's
		// error message, adapted for success.
		headerStyle := lipgloss.NewStyle().
			Foreground(charmtone.Butter).
			Background(charmtone.Guac).
			Bold(true).
			Padding(0, 1).
			Margin(1).
			MarginLeft(2).
			SetString("SUCCESS")
		textStyle := lipgloss.NewStyle().
			MarginLeft(2).
			SetString(fmt.Sprintf("%s provider updated successfully.", updateProvidersSource))

		fmt.Printf("%s\n%s\n\n", headerStyle.Render(), textStyle.Render())
		return nil
	},
}

func init() {
	updateProvidersCmd.Flags().StringVar(&updateProvidersSource, "source", "catwalk", "Provider source to update (catwalk)")
}
