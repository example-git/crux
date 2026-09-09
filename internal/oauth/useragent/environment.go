package useragent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func environmentOrForContext(ctx context.Context, key, fallback string) string {
	value, _ := oauth.LookupEnvironment(ctx, key)
	if value != "" {
		return value
	}
	return fallback
}

func userHomeForContext(ctx context.Context) (string, error) {
	if _, bound := oauth.EnvironmentFromContext(ctx); !bound {
		return os.UserHomeDir()
	}
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	if home := environmentOrForContext(ctx, key, ""); home != "" {
		return home, nil
	}
	return "", errors.New("captured OAuth environment has no home directory")
}

func persistedForContext(ctx context.Context, key string) string {
	if _, bound := oauth.EnvironmentFromContext(ctx); !bound {
		return persisted(key)
	}
	home, err := userHomeForContext(ctx)
	if err != nil {
		return ""
	}
	persistMu.Lock()
	defer persistMu.Unlock()
	data, err := os.ReadFile(filepath.Join(home, ".ai-cli", "useragent-versions.json"))
	if err != nil {
		return ""
	}
	var versions map[string]string
	if json.Unmarshal(data, &versions) != nil || !fullVersionRe.MatchString(versions[key]) {
		return ""
	}
	return versions[key]
}

// Probe commands inherit only the environment bound to the owner. In
// particular an absent PATH cannot select an executable from the process PATH.
func commandOutputForContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	return commandResultForContext(ctx, false, name, args...)
}

func commandCombinedOutputForContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	return commandResultForContext(ctx, true, name, args...)
}

func commandResultForContext(ctx context.Context, combined bool, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return nil, err
	}
	entries, bound := oauth.EnvironmentFromContext(ctx)
	if !bound {
		command := exec.CommandContext(ctx, name, args...)
		if combined {
			return command.CombinedOutput()
		}
		return command.Output()
	}
	path := environmentOrForContext(ctx, "PATH", "")
	if path == "" && !filepath.IsAbs(name) {
		return nil, exec.ErrNotFound
	}
	directory := environmentOrForContext(ctx, "PWD", "")
	resolved, err := interp.LookPathDir(directory, expand.ListEnviron(entries...), name)
	if err != nil {
		return nil, err
	}
	// Avoid exec.CommandContext performing a second lookup against process PATH.
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return nil, err
		}
	}
	command := exec.CommandContext(ctx, resolved, args...)
	command.Env = entries
	command.Dir = directory
	var output []byte
	if combined {
		output, err = command.CombinedOutput()
	} else {
		output, err = command.Output()
	}
	if ownerErr := providertransport.ValidateContextOwner(ctx); ownerErr != nil {
		return nil, ownerErr
	}
	return output, err
}

func copilotAdvertisementModeForContext(ctx context.Context) CopilotMode {
	switch strings.ToLower(environmentOrForContext(ctx, "COPILOT_ADVERTISE_MODE", "")) {
	case "cli", "copilot-cli":
		return CopilotModeCLI
	default:
		return CopilotModeVSCode
	}
}
