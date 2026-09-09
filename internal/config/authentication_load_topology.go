package config

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/fsext"
)

type authenticationConfigWrite struct {
	path    string
	fields  map[string]any
	removed []string
}

// prepareAuthenticationLoadTopology projects only the loader's fixed writes.
// All bytes come from retained reads plus exact field receipts, never a later
// file observation. The returned basis is complete before runtime preparation.
func prepareAuthenticationLoadTopology(ctx context.Context, original *authenticationLoadBasis, notification notificationMigrationPlan, modelFields, migrationFields map[string]any, globalPath, workingDir, workspacePath string, base env.Env) (*authenticationLoadBasis, []string, error) {
	writes := []authenticationConfigWrite{}
	for _, path := range slices.Sorted(maps.Keys(notification.overrides)) {
		write := authenticationConfigWrite{path: path, fields: map[string]any{}}
		if notification.setNotifications && path == notification.dataConfig {
			write.fields["options.notifications"] = notification.value
		}
		if notification.cleanPaths[path] {
			write.removed = []string{"options.disable_notifications", "options.notification_style"}
		}
		writes = append(writes, write)
	}
	if len(modelFields) > 0 {
		writes = append(writes, authenticationConfigWrite{path: globalPath, fields: modelFields})
	}
	if len(migrationFields) > 0 {
		writes = append(writes, authenticationConfigWrite{path: globalPath, fields: migrationFields})
	}
	return projectAuthenticationBasisWrites(ctx, original, writes, workingDir, workspacePath, base)
}

// projectAuthenticationBasisWrites serves startup and ordinary typed writes.
// Its result belongs only to an unpublished candidate until the actual writes
// have been verified; no later observation can advance these retained sources.
func projectAuthenticationBasisWrites(ctx context.Context, original *authenticationLoadBasis, writes []authenticationConfigWrite, workingDir, workspacePath string, base env.Env) (*authenticationLoadBasis, []string, error) {
	if original == nil || len(writes) == 0 {
		return original, nil, nil
	}
	paths := []string{}
	for _, write := range writes {
		if !slices.Contains(paths, write.path) {
			paths = append(paths, write.path)
		}
	}
	globals, err := authenticationGlobalInputPaths(base)
	if err != nil {
		return nil, nil, err
	}
	boundary := workingDir
	if cached, ok := worktreeRootCache.Load(workingDir); ok && cached.(string) != "" {
		boundary = cached.(string)
	}
	current, projected, err := fsext.LookupBoundedWithCreatedFiles(ctx, workingDir, boundary, paths,
		"."+appName+"rc", appName+"rc", "."+appName+".json", appName+".json")
	if err != nil {
		return nil, nil, err
	}
	slices.Reverse(current)
	slices.Reverse(projected)
	current = append(append(slices.Clone(globals), current...), workspacePath)
	projected = append(append(slices.Clone(globals), projected...), workspacePath)
	for i := range current {
		current[i] = filepath.Clean(current[i])
	}
	for i := range projected {
		projected[i] = filepath.Clean(projected[i])
	}
	if !slices.Equal(original.order, current) {
		return nil, nil, errAuthenticationInputsChanged
	}
	next := original.clone()
	next.order = projected
	allPaths := slices.Clone(projected)
	for path := range original.sources {
		if !slices.Contains(allPaths, path) {
			allPaths = append(allPaths, path)
		}
	}
	aliasesByTarget, err := fsext.PathsReadingCreatedFiles(ctx, paths, allPaths)
	if err != nil {
		return nil, nil, err
	}
	writtenPaths := []string{}
	for _, write := range writes {
		source, found := next.sources[filepath.Clean(write.path)]
		if !found {
			return nil, nil, errAuthenticationBasisUnavailable
		}
		aliases := aliasesByTarget[write.path]
		for _, path := range aliases {
			if _, found := next.sources[path]; !found {
				// A newly discovered alias reads the same old target; the exact
				// authored fields below are its only source of new content.
				next.sources[path] = source
			}
			next = next.authored(path, write.fields, write.removed)
			if !slices.Contains(writtenPaths, path) {
				writtenPaths = append(writtenPaths, path)
			}
		}
	}
	for _, path := range next.order {
		if _, found := next.sources[path]; !found {
			return nil, nil, errAuthenticationInputsChanged
		}
	}
	return next, writtenPaths, ctx.Err()
}

// verifyAuthenticationWriteTopology checks the predicted order and authored
// source postimages. It never advances the prepared basis or evaluates shell.
func verifyAuthenticationWriteTopology(ctx context.Context, basis *authenticationLoadBasis, writtenPaths []string, workingDir, workspacePath string, base env.Env) error {
	if len(writtenPaths) == 0 {
		return nil
	}
	order := append(lookupConfigsFromEnvironment(workingDir, base), workspacePath)
	for i := range order {
		order[i] = filepath.Clean(order[i])
	}
	if basis == nil || !slices.Equal(order, basis.order) {
		return errAuthenticationInputsChanged
	}
	for _, path := range writtenPaths {
		file, err := readAuthenticationInput(ctx, path)
		if err != nil {
			return err
		}
		expected, found := basis.sources[path]
		if !found || file.info.exists != expected.exists || isShellConfig(path) && !bytes.Equal(expected.raw, file.data) || !isShellConfig(path) && !authenticationBasisJSONEqual(expected.raw, file.data, "") {
			return errors.New("authored configuration postimage differs from prepared authentication inputs")
		}
	}
	return ctx.Err()
}
