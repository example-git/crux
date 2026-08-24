package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/example-git/crux/internal/backup"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var readBackupPassword = term.ReadPassword

var exportCmd = &cobra.Command{
	Use:   "export [archive]",
	Short: "Export provider, plugin, and account data to an encrypted archive",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		outputPath := "crux-backup-" + time.Now().UTC().Format("20060102T150405Z") + ".crux"
		if len(args) == 1 {
			outputPath = args[0]
		}
		password, err := promptBackupPassword(cmd, "Backup password: ")
		if err != nil {
			return err
		}
		defer clearBytes(password)
		confirmation, err := promptBackupPassword(cmd, "Confirm password: ")
		if err != nil {
			return err
		}
		defer clearBytes(confirmation)
		if !bytes.Equal(password, confirmation) {
			return errors.New("passwords do not match")
		}
		result, err := backup.Export(outputPath, password)
		if err != nil {
			return err
		}
		cmd.Printf("Exported %d files to %s.\n", result.Files, outputPath)
		return nil
	},
}

var importCmd = &cobra.Command{
	Use:   "import <archive>",
	Short: "Import provider, plugin, and account data from an encrypted archive",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		password, err := promptBackupPassword(cmd, "Backup password: ")
		if err != nil {
			return err
		}
		defer clearBytes(password)
		result, err := backup.Import(args[0], password)
		if err != nil {
			return err
		}
		cmd.Printf("Imported %d files. Restart Crux to load restored providers and accounts.\n", result.Files)
		return nil
	},
}

func promptBackupPassword(cmd *cobra.Command, prompt string) ([]byte, error) {
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
		return nil, err
	}
	password, err := readBackupPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return nil, fmt.Errorf("read password: %w", err)
	}
	if len(password) == 0 {
		return nil, errors.New("password cannot be empty")
	}
	return password, nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
