package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/fsext"
)

// authenticationScopeTopology predicts only the authored file replacement.
// inputs retains preimage bytes/identities, including for writtenPaths. The
// transaction must install the exact staged bytes in each written source and
// finalize order before preparing a runtime. After writing, it substitutes the
// verified postimage at writtenPaths and checks the entire observation again.
// This is neither a file freshness fence nor an accepted configuration basis.
type authenticationScopeTopology struct {
	inputs       authenticationConfigInputs
	writtenPaths []string
}

func (authenticationScopeTopology) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication scope topology is private")
}
func (authenticationScopeTopology) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication scope topology]"))
}

// prepareAuthenticationScopeTopology uses the normal bounded lookup to predict
// how renaming a regular file over path affects discovery. It reads metadata
// only: no configuration bytes, shell execution, git discovery, or live env.
// The caller holds the store's writeMu and supplies its captured paths/env.
func prepareAuthenticationScopeTopology(ctx context.Context, snapshot RuntimeSnapshot, inputs authenticationConfigInputs, path, workingDir, workspacePath string, base env.Env) (authenticationScopeTopology, error) {
	if snapshot.IsClientOwned() {
		return authenticationScopeTopology{}, ErrClientRuntimeManaged
	}
	if err := ctx.Err(); err != nil {
		return authenticationScopeTopology{}, err
	}
	if !inputs.valid || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return authenticationScopeTopology{}, errors.New("authentication scope topology requires a captured absolute target")
	}
	target, captured := inputs.file(path)
	if !captured {
		return authenticationScopeTopology{}, errors.New("authentication scope target was not captured")
	}
	var currentOrder, projectedOrder []string
	if workingDir != "" {
		if !filepath.IsAbs(workingDir) {
			return authenticationScopeTopology{}, errors.New("authentication working directory is not absolute")
		}
		if base == nil {
			base = snapshot.environment
		}
		globals, err := authenticationGlobalInputPaths(base)
		if err != nil {
			return authenticationScopeTopology{}, authenticationInputError(err)
		}
		currentOrder = append(currentOrder, globals...)
		projectedOrder = append(projectedOrder, globals...)
		boundary := workingDir
		if cached, ok := worktreeRootCache.Load(workingDir); ok && cached.(string) != "" {
			boundary = cached.(string)
		}
		current, projected, err := fsext.LookupBoundedWithCreatedFile(ctx, workingDir, boundary, path,
			"."+appName+"rc", appName+"rc", "."+appName+".json", appName+".json")
		if err != nil {
			return authenticationScopeTopology{}, authenticationInputError(err)
		}
		slices.Reverse(current)
		slices.Reverse(projected)
		currentOrder = append(currentOrder, current...)
		projectedOrder = append(projectedOrder, projected...)
	}
	if workspacePath != "" {
		if !filepath.IsAbs(workspacePath) {
			return authenticationScopeTopology{}, errors.New("authentication workspace path is not absolute")
		}
		currentOrder = append(currentOrder, workspacePath)
		projectedOrder = append(projectedOrder, workspacePath)
	}
	for i := range currentOrder {
		currentOrder[i] = filepath.Clean(currentOrder[i])
	}
	for i := range projectedOrder {
		projectedOrder[i] = filepath.Clean(projectedOrder[i])
	}
	if !slices.Equal(inputs.order, currentOrder) {
		return authenticationScopeTopology{}, errAuthenticationInputsChanged
	}
	// Discovery order comes first; previously observed loaded/persistence paths
	// remain in their original tail order. Keep duplicate layer occurrences but
	// only one observation of each lexical path, just like normal capture.
	paths := slices.Clone(projectedOrder)
	for _, file := range inputs.files {
		if !slices.Contains(paths, file.path) {
			paths = append(paths, file.path)
		}
	}
	written, err := fsext.PathsReadingCreatedFile(ctx, path, paths)
	if err != nil {
		return authenticationScopeTopology{}, authenticationInputError(err)
	}
	if !slices.Contains(written, path) {
		return authenticationScopeTopology{}, errAuthenticationInputsChanged
	}
	result := authenticationScopeTopology{
		inputs:       authenticationConfigInputs{valid: true, order: projectedOrder},
		writtenPaths: written,
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		file, captured := inputs.file(path)
		if slices.Contains(written, path) {
			if captured && (file.info != target.info || !bytes.Equal(file.data, target.data)) {
				return authenticationScopeTopology{}, errAuthenticationInputsChanged
			}
			if !captured {
				// A newly discovered alias observes the same old entry as the
				// captured target. Retain that preimage, not fabricated new bytes.
				file = target
				file.path = path
				file.data = bytes.Clone(target.data)
			}
		} else if !captured {
			return authenticationScopeTopology{}, errAuthenticationInputsChanged
		}
		result.inputs.files = append(result.inputs.files, file)
	}
	return result, ctx.Err()
}
