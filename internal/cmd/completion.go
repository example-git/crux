package cmd

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/example-git/crux/internal/version"
	"github.com/spf13/cobra"
)

func init() { configureShellCompletions(rootCmd) }

func configureShellCompletions(root *cobra.Command) {
	// Explicit arguments also create the command before other init functions
	// have registered their subcommands. Cobra retains the standard shell
	// commands, argument validation, and --no-descriptions flags.
	root.InitDefaultCompletionCmd("completion")
	completion, _, _ := root.Find([]string{"completion"})
	completion.Args = cobra.NoArgs
	completion.Flags().String("install", "", "Append the completion loading line to the startup file (bash, zsh, fish)")
	_ = completion.RegisterFlagCompletionFunc("install", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"bash", "zsh", "fish"}, cobra.ShellCompDirectiveNoFileComp
	})
	completion.RunE = func(command *cobra.Command, _ []string) error {
		if !command.Flags().Changed("install") {
			return command.Help()
		}
		shell, _ := command.Flags().GetString("install")
		return installShellCompletion(command.Root().Name(), shell, command.OutOrStdout())
	}
	for _, shell := range completion.Commands() {
		shell.RunE = generateShellCompletion
	}
	configureShellCompletionHelp(root)
}

func configureShellCompletionHelp(root *cobra.Command) {
	completion, _, _ := root.Find([]string{"completion"})
	completion.Long = fmt.Sprintf(`Generate shell completions from the current Crux command tree.

Load them directly from your shell startup file, or append the loading line once:

  %[1]s completion --install zsh
  %[1]s completion --install bash
  %[1]s completion --install fish

Installation preserves existing contents and skips a loading line already present.
The command printed afterward loads completions into your current shell immediately.`, root.Name())
	for _, shell := range completion.Commands() {
		var setup string
		switch shell.Name() {
		case "zsh":
			setup = fmt.Sprintf(`Add this line to ~/.zshrc:

  eval "$(%s completion zsh)"

The script initializes Zsh completion support only if it is not already enabled.`, root.Name())
		case "bash":
			setup = fmt.Sprintf(`Add this line to ~/.bashrc:

  eval "$(%s completion bash)"`, root.Name())
		case "fish":
			setup = fmt.Sprintf(`Add this line to ~/.config/fish/config.fish (or $XDG_CONFIG_HOME/fish/config.fish):

  %s completion fish | source`, root.Name())
		case "powershell":
			setup = fmt.Sprintf(`Add this line to your PowerShell profile:

  %s completion powershell | Out-String | Invoke-Expression`, root.Name())
		default:
			continue
		}
		shell.Long = "Generate shell completions from the current Crux command tree.\n\n" + setup +
			"\n\nScript generation and Tab-completion requests skip normal application startup."
	}
}

func generateShellCompletion(command *cobra.Command, _ []string) error {
	noDescriptions, _ := command.Flags().GetBool("no-descriptions")
	out, root := command.OutOrStdout(), command.Root()
	switch command.Name() {
	case "bash":
		if err := root.GenBashCompletionV2(out, !noDescriptions); err != nil {
			return err
		}
		return writeStandaloneBashCompletion(out, root.Name())
	case "zsh":
		// Keep #compdef first for users who also load this through fpath.
		// Direct eval needs compdef available before Cobra's registration.
		if _, err := fmt.Fprintf(out, "#compdef %s\n\nif (( ! $+functions[compdef] )); then\n  autoload -Uz compinit\n  compinit\nfi\n\n", root.Name()); err != nil {
			return err
		}
		if noDescriptions {
			return root.GenZshCompletionNoDesc(out)
		}
		return root.GenZshCompletion(out)
	case "fish":
		return root.GenFishCompletion(out, !noDescriptions)
	case "powershell":
		if noDescriptions {
			return root.GenPowerShellCompletion(out)
		}
		return root.GenPowerShellCompletionWithDesc(out)
	}
	return fmt.Errorf("unsupported completion shell %q", command.Name())
}

// ExecuteShellCompletion handles only shell-script generation and Cobra's
// hidden Tab-completion requests. main calls it before dotenv, compatibility
// dispatch, profiling, or the styled CLI setup. Suggestions come from the same
// command tree used for execution; no generated command catalogue can go stale.
func ExecuteShellCompletion(executable string, args []string, stdout, stderr io.Writer) (bool, error) {
	initializeProviderCompletions()
	return executeShellCompletion(rootCmd, executable, args, stdout, stderr)
}

func executeShellCompletion(root *cobra.Command, executable string, args []string, stdout, stderr io.Writer) (bool, error) {
	// Find understands flag values and '--', unlike scanning for a token
	// named "completion". Temporary nodes let it also recognize hidden
	// completion commands before Execute initializes their actual handlers.
	var probes []*cobra.Command
	for _, name := range []string{cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd} {
		found := false
		for _, child := range root.Commands() {
			found = found || child.Name() == name
		}
		if !found {
			probe := &cobra.Command{Use: name, Hidden: true, Args: cobra.ArbitraryArgs}
			root.AddCommand(probe)
			probes = append(probes, probe)
		}
	}
	target, _, _ := root.Find(args)
	handled := false
	for node := target; node != nil && node != root; node = node.Parent() {
		handled = handled || node.Name() == "completion" || node.Name() == cobra.ShellCompRequestCmd || node.Name() == cobra.ShellCompNoDescRequestCmd
	}
	root.RemoveCommand(probes...)
	if !handled {
		return false, nil
	}
	// argv[0] preserves the invoked name, including a renamed executable or
	// symlink. Resolving the target would register the wrong shell command.
	root.Use = filepath.Base(executable)
	configureShellCompletionHelp(root)
	root.Version = version.Version
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return true, root.Execute()
}
