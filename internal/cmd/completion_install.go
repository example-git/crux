package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func completionSetup(executable, shell string) (line, path string, err error) {
	// Validate before looking up or creating any directories. In particular,
	// an explicit empty or unknown shell must not select a default shell.
	switch shell {
	case "bash", "zsh":
		line = fmt.Sprintf(`eval "$(%s completion %s)"`, executable, shell)
	case "fish":
		line = executable + " completion fish | source"
	default:
		return "", "", fmt.Errorf("unsupported installation shell %q: choose bash, zsh, or fish", shell)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("locate shell startup file: %w", err)
	}
	path = filepath.Join(home, "."+shell+"rc")
	if shell == "fish" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		path = filepath.Join(configHome, "fish", "config.fish")
	}
	return line, path, nil
}

func installShellCompletion(executable, shell string, out io.Writer) error {
	line, path, err := completionSetup(executable, shell)
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read shell startup file: %w", err)
	}
	for _, existing := range strings.Split(string(contents), "\n") {
		// Ignore surrounding whitespace and a trailing comment, but do not
		// mistake a commented-out command for an installed completion loader.
		command, _, _ := strings.Cut(existing, "#")
		if strings.TrimSpace(command) == line {
			_, err = fmt.Fprintf(out, "Completions already configured in %s.\n\nLoad them in this shell now:\n  %s\n", path, line)
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create shell configuration directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open shell startup file: %w", err)
	}
	addition := line + "\n"
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		addition = "\n" + addition
	}
	_, writeErr := io.WriteString(file, addition)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("append completion loading line: %w", err)
	}
	_, err = fmt.Fprintf(out, "Completions configured in %s.\n\nLoad them in this shell now:\n  %s\n", path, line)
	return err
}
