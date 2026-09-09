package config

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/example-git/crux/internal/env"
)

// previewScopedConfigWrite follows the same authored replacement topology as
// persistence. An alias of the write target must receive the staged bytes too;
// its old value is not a higher-scope override. No account I/O or new shell
// evaluation is introduced before the existing preview load.
func (s *ConfigStore) previewScopedConfigWrite(ctx context.Context, path string, before []byte, existed bool, after []byte, base env.Env, dataDir string) (*Config, error) {
	s.configMu.Lock()
	snapshot := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	inputs, err := s.captureAuthenticationInputsLocked(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	target, captured := inputs.file(path)
	if !captured || target.info.exists != existed || existed && !bytes.Equal(target.data, before) {
		return nil, errors.New("config source changed before preview")
	}
	topology, err := prepareAuthenticationScopeTopology(ctx, snapshot, inputs, path, s.workingDir, s.workspacePath, base)
	if err != nil {
		return nil, err
	}
	paths := slices.Clone(topology.inputs.order)
	if s.workspacePath != "" {
		paths = paths[:len(paths)-1]
	}
	overrides := make(map[string][]byte, len(topology.writtenPaths))
	for _, alias := range topology.writtenPaths {
		overrides[alias] = after
	}
	preview, _, _, err := s.loadReloadConfigInputs(ctx, paths, overrides, base, dataDir, s.ephemeralProviderSnapshot(), cloneRuntimeOverrides(s.overrides), true)
	return preview, err
}
