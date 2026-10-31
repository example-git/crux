package config

import (
	"context"
	"path/filepath"

	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/fsext"
)

func CompletionDataDirectory(ctx context.Context, workingDir, dataDir string) (string, error) {
	if dataDir != "" {
		return resolveDataDirectory(workingDir, dataDir, ""), nil
	}
	environment := snapshotEnvironment()
	cfg, _, err := loadFromConfigPathsWithOverrides(ctx, lookupConfigsFromEnvironment(workingDir, environment), nil, environment)
	if err != nil {
		return "", err
	}
	configured := ""
	if cfg.Options != nil {
		configured = cfg.Options.DataDirectory
	}
	return resolveDataDirectory(workingDir, "", configured), nil
}

func resolveDataDirectory(workingDir, dataDir, configured string) string {
	if dataDir == "" {
		dataDir = configured
	}
	if dataDir == "" {
		if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
			dataDir = path
		} else {
			dataDir = filepath.Join(workingDir, defaultDataDirectory)
		}
	}
	return filepath.Clean(filepathext.SmartJoin(workingDir, dataDir))
}
