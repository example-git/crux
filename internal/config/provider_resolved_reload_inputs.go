package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/example-git/crux/internal/env"
)

type resolvedReloadInputs struct {
	inputs        authenticationConfigInputs
	snapshot      RuntimeSnapshot
	workspacePath string
}

func (resolvedReloadInputs) MarshalJSON() ([]byte, error) {
	return nil, errors.New("resolved reload inputs are private")
}

func (resolvedReloadInputs) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private resolved reload inputs]"))
}

func configHasResolvedInputs(cfg *Config) bool {
	for _, provider := range resolvedInputProviders(cfg) {
		if provider.resolvedAPIKey != nil || provider.resolvedEndpoint != nil || len(provider.resolvedCredentials) > 0 {
			return true
		}
	}
	return false
}

// The generic setter has already authored its fixed fields. Bind the next
// reload to the exact source observation it actually read, before provider
// preparation can run and before exposing the immutable runtime candidate.
func (s *ConfigStore) captureResolvedReloadInputs(ctx context.Context, cfg *Config, basis *authenticationLoadBasis, workspacePath string, base env.Env, notification notificationMigrationPlan) (resolvedReloadInputs, error) {
	observed := resolvedReloadInputs{snapshot: RuntimeSnapshot{config: cfg, environment: base}, workspacePath: workspacePath}
	var err error
	observed.inputs, err = s.captureAuthenticationInputsLocked(ctx, observed.snapshot, workspacePath)
	if err != nil {
		return resolvedReloadInputs{}, err
	}
	if basis == nil || !basis.valid || !slices.Equal(observed.inputs.order, basis.order) {
		return resolvedReloadInputs{}, errAuthenticationInputsChanged
	}
	for _, path := range basis.order {
		file, found := observed.inputs.file(path)
		source, known := basis.sources[path]
		expected, exists := file.data, file.info.exists
		if override, overridden := notification.overrides[path]; overridden {
			original, captured := notification.sources[path]
			if !captured || original.exists != file.info.exists || !bytes.Equal(original.data, file.data) {
				return resolvedReloadInputs{}, errAuthenticationInputsChanged
			}
			expected, exists = override, true
		}
		if !found || !known || exists != source.exists || !bytes.Equal(expected, source.raw) {
			return resolvedReloadInputs{}, errAuthenticationInputsChanged
		}
	}
	return observed, nil
}

// Before startup writes, every file must be unchanged. After the reload's own
// validated startup/migration receipts, only those authored paths may differ;
// their intended source values are already frozen on the prepared basis.
func (s *ConfigStore) verifyResolvedReloadInputs(ctx context.Context, observed resolvedReloadInputs, basis *authenticationLoadBasis, authored []string) error {
	current, err := s.captureAuthenticationInputsLocked(ctx, observed.snapshot, observed.workspacePath)
	if err != nil {
		return err
	}
	if len(authored) == 0 {
		if !observed.inputs.sameObservation(current) {
			return errAuthenticationInputsChanged
		}
		return ctx.Err()
	}
	if basis == nil || !basis.valid || !slices.Equal(current.order, basis.order) {
		return errAuthenticationInputsChanged
	}
	for _, file := range current.files {
		if slices.Contains(authored, file.path) {
			source, found := basis.sources[file.path]
			if !found || file.info.exists != source.exists || isShellConfig(file.path) && !bytes.Equal(file.data, source.raw) || !isShellConfig(file.path) && !authenticationBasisJSONEqual(file.data, source.raw, "") {
				return errAuthenticationInputsChanged
			}
		} else {
			original, found := observed.inputs.file(file.path)
			if !found || original.info != file.info || !bytes.Equal(original.data, file.data) {
				return errAuthenticationInputsChanged
			}
		}
	}
	for _, file := range observed.inputs.files {
		if _, found := current.file(file.path); !found {
			return errAuthenticationInputsChanged
		}
	}
	return ctx.Err()
}
