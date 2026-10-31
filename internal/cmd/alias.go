package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/example-git/crux/internal/compatibility/install"
	"github.com/spf13/cobra"
)

func newAliasCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "alias",
		Short: "Manage provider CLI aliases for this Crux executable",
		Args:  cobra.NoArgs,
	}
	command.PersistentFlags().String("root", "", "Private alias installation directory")
	_ = command.MarkPersistentFlagDirname("root")
	for _, operation := range []string{"install", "uninstall", "status", "enable", "disable", "repair"} {
		child := &cobra.Command{
			Use:   operation,
			Short: aliasOperationDescription(operation),
			Args:  cobra.NoArgs,
			RunE:  runAliasCommand,
		}
		if operation == "install" || operation == "uninstall" {
			child.Use += " [PROVIDERNAME]"
			child.Long = child.Short + ".\n\nSelect one provider (codex, claude, antigravity, or copilot) or use --all.\nAntigravity installs the agy command; agy is also accepted as a provider name."
			child.Flags().Bool("all", false, "Select all supported provider aliases")
			child.Args = func(cmd *cobra.Command, args []string) error {
				_, err := aliasSelection(cmd, args)
				return err
			}
			child.ValidArgsFunction = func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
				all, _ := cmd.Flags().GetBool("all")
				if all || len(args) != 0 {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				return []string{"codex", "claude", "antigravity", "agy", "copilot"}, cobra.ShellCompDirectiveNoFileComp
			}
		}
		if operation == "install" || operation == "repair" {
			child.Flags().String("shell", "", "Shell for PATH setup (bash, zsh, fish)")
			child.Flags().String("profile", "", "Shell profile to update")
			child.Flags().Bool("skip-path", false, "Do not configure PATH")
			_ = child.MarkFlagFilename("profile")
			_ = child.RegisterFlagCompletionFunc("shell", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
				return []string{"bash", "zsh", "fish"}, cobra.ShellCompDirectiveNoFileComp
			})
		}
		command.AddCommand(child)
	}
	return command
}

func aliasOperationDescription(operation string) string {
	switch operation {
	case "install":
		return "Install aliases using this Crux executable"
	case "uninstall":
		return "Remove managed aliases and clean up PATH when none remain"
	case "status":
		return "Show alias installation and PATH status"
	case "enable":
		return "Enable Crux compatibility for installed aliases"
	case "disable":
		return "Forward installed aliases to the original tools on PATH"
	default:
		return "Repair installed aliases using this Crux executable"
	}
}

func aliasSelection(cmd *cobra.Command, args []string) ([]string, error) {
	all, err := cmd.Flags().GetBool("all")
	if err != nil {
		return nil, err
	}
	if all && len(args) == 0 {
		return install.CanonicalAliases(), nil
	}
	if all || len(args) != 1 {
		return nil, fmt.Errorf("specify either --all or one provider name: codex, claude, antigravity, or copilot")
	}
	name := strings.ToLower(args[0])
	switch name {
	case "antigravity", "gemini-ag":
		name = "agy"
	case "claude-ai":
		name = "claude"
	}
	for _, supported := range install.CanonicalAliases() {
		if name == supported {
			return []string{name}, nil
		}
	}
	return nil, fmt.Errorf("unsupported alias provider %q; expected codex, claude, antigravity, or copilot", args[0])
}

func runAliasCommand(cmd *cobra.Command, args []string) error {
	root, _ := cmd.Flags().GetString("root")
	manager, err := install.New(root)
	if err != nil {
		return err
	}
	var status install.Status
	switch cmd.Name() {
	case "install", "repair":
		var executable string
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate the running Crux executable: %w", err)
		}
		options := install.Options{Executable: executable, VerifyCompatibility: true}
		options.Shell, _ = cmd.Flags().GetString("shell")
		options.Profile, _ = cmd.Flags().GetString("profile")
		options.SkipPath, _ = cmd.Flags().GetBool("skip-path")
		if cmd.Name() == "install" {
			options.Aliases, err = aliasSelection(cmd, args)
			if err != nil {
				return err
			}
			status, err = manager.Install(options)
		} else {
			status, err = manager.Repair(options)
		}
	case "uninstall":
		names, selectionErr := aliasSelection(cmd, args)
		if selectionErr != nil {
			return selectionErr
		}
		status, err = manager.Uninstall(names...)
	case "status":
		status, err = manager.Status()
	case "enable":
		status, err = manager.Enable()
	case "disable":
		status, err = manager.Disable()
	}
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "root: %s\nbin: %s\n", status.Root, status.Bin)
	if status.Executable != "" {
		fmt.Fprintf(out, "executable: %s\n", status.Executable)
	}
	fmt.Fprintf(out, "installed: %t\nenabled: %t\npath setup: %t\npath active: %t\n", status.Installed, status.Enabled, status.PathSetup, status.PathActive)
	if status.Profile != "" {
		fmt.Fprintf(out, "profile: %s\n", status.Profile)
	}
	for _, alias := range status.Aliases {
		fmt.Fprintf(out, "%s: %s (%s)\n", alias.Name, alias.State, alias.Path)
	}
	return nil
}
