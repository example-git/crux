package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	cruxlog "github.com/example-git/crux/internal/log"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "crux-command-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cleanup := cruxlog.Setup(filepath.Join(root, "crux.log"), false)
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
