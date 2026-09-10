package config

import "context"

// prepareAuthenticationCOW finalizes only an unpublished clone's basis. The
// caller holds writeMu, and must verify the receipt before publishing next.
// Manual stores without an accepted load basis retain that explicit absence.
func (s *ConfigStore) prepareAuthenticationCOW(ctx context.Context, next *Config, path string, fields map[string]any, removed []string) ([]string, error) {
	if next.authenticationBasis == nil {
		return nil, nil
	}
	basis, written, err := projectAuthenticationBasisWrites(ctx, next.authenticationBasis, []authenticationConfigWrite{{path: path, fields: fields, removed: removed}}, s.workingDir, s.workspacePath, s.baseEnvironment, s.globalOnly)
	if err != nil {
		return nil, err
	}
	next.authenticationBasis = basis
	return written, nil
}

func (s *ConfigStore) verifyAuthenticationCOW(ctx context.Context, next *Config, written []string) error {
	return verifyAuthenticationWriteTopology(ctx, next.authenticationBasis, written, s.workingDir, s.workspacePath, s.baseEnvironment, s.globalOnly)
}
