package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	compatinstall "github.com/example-git/crux/internal/compatibility/install"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crux-compat <install|enable|disable|status|repair|uninstall> [options]")
	}
	operation := args[0]
	flags := flag.NewFlagSet("crux-compat "+operation, flag.ContinueOnError)
	root := flags.String("root", "", "private compatibility directory")
	executable := flags.String("executable", "", "Crux executable to hard-link")
	shell := flags.String("shell", "", "shell for PATH setup")
	profile := flags.String("profile", "", "shell profile to update")
	skipPath := flags.Bool("skip-path", false, "do not configure PATH")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	manager, err := compatinstall.New(*root)
	if err != nil {
		return err
	}
	options := compatinstall.Options{
		Executable:          *executable,
		Shell:               *shell,
		Profile:             *profile,
		SkipPath:            *skipPath,
		VerifyCompatibility: operation == "install" || operation == "repair",
	}
	if operation == "install" && options.Executable == "" {
		options.Executable, err = exec.LookPath("crux")
		if err != nil {
			return errors.New("find Crux executable in PATH; use --executable to select it explicitly")
		}
	}
	var status compatinstall.Status
	switch operation {
	case "install":
		status, err = manager.Install(options)
	case "enable", "on":
		status, err = manager.Enable()
	case "disable", "off":
		status, err = manager.Disable()
	case "status":
		status, err = manager.Status()
	case "repair":
		status, err = manager.Repair(options)
	case "uninstall":
		status, err = manager.Uninstall()
	default:
		return fmt.Errorf("unknown operation %q; expected install, enable, disable, status, repair, or uninstall", operation)
	}
	if err != nil {
		return err
	}
	printStatus(status)
	return nil
}

func printStatus(status compatinstall.Status) {
	fmt.Printf("root: %s\n", status.Root)
	fmt.Printf("bin: %s\n", status.Bin)
	if status.Executable != "" {
		fmt.Printf("executable: %s\n", status.Executable)
	}
	fmt.Printf("installed: %t\n", status.Installed)
	fmt.Printf("enabled: %t\n", status.Enabled)
	fmt.Printf("path setup: %t\n", status.PathSetup)
	fmt.Printf("path active: %t\n", status.PathActive)
	if status.Profile != "" {
		fmt.Printf("profile: %s\n", status.Profile)
	}
	for _, alias := range status.Aliases {
		fmt.Printf("%s: %s (%s)\n", alias.Name, alias.State, alias.Path)
	}
}
