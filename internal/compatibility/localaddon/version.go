package localaddon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/useragent"
)

func compatibilityVersionOutput(ctx context.Context, invocation compatibility.Invocation, name string) error {
	environment := invocation.Env
	if environment == nil {
		environment = os.Environ()
	}
	ctx = oauth.ContextWithEnvironment(ctx, environment)
	var version, format string
	var err error
	switch name {
	case "codex":
		version, err = useragent.CodexVersionForContext(ctx)
		format = "codex-cli %s\n"
	case "agy":
		version, err = useragent.GeminiVersionForContext(ctx)
		format = "%s\n"
	case "copilot":
		version, err = useragent.CopilotCLIVersionForContext(ctx)
		format = "GitHub Copilot CLI %s\n"
	case "claude":
		version, err = claudeClientVersion(ctx, invocation.WorkingDir, environment)
		format = "%s (Claude Code)\n"
	default:
		return fmt.Errorf("unsupported compatibility version target %q", name)
	}
	if err != nil {
		return fmt.Errorf("resolve %s client version: %w", name, err)
	}
	return successOutput(fmt.Sprintf(format, version), false)
}

func codexInitializeUserAgent(ctx context.Context, invocation compatibility.Invocation) (string, error) {
	environment := invocation.Env
	if environment == nil {
		environment = os.Environ()
	}
	return useragent.CodexForContext(oauth.ContextWithEnvironment(ctx, environment))
}

func claudeClientVersion(ctx context.Context, workingDir string, environment []string) (string, error) {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	return config.ResolveProviderClientVersion(ctx, workingDir, "claude-ai", env.NewFromMap(values))
}
