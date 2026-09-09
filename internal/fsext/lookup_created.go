package fsext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// LookupBoundedWithCreatedFile returns ordinary discovery and discovery after
// renaming a new regular file over createdPath. It performs the same traversal,
// target ordering and ownership checks as LookupBounded, without creating or
// reading file contents. The projection is preparation, not a freshness proof.
func LookupBoundedWithCreatedFile(ctx context.Context, dir, stopDir, createdPath string, targets ...string) (current, projected []string, err error) {
	return LookupBoundedWithCreatedFiles(ctx, dir, stopDir, []string{createdPath}, targets...)
}

// LookupBoundedWithCreatedFiles projects a fixed set of authored replacements
// in one ordinary traversal. It neither writes files nor reads their contents.
func LookupBoundedWithCreatedFiles(ctx context.Context, dir, stopDir string, createdPaths []string, targets ...string) (current, projected []string, err error) {
	replacements := make([]createdFile, 0, len(createdPaths))
	for _, path := range createdPaths {
		replacement, err := newCreatedFile(ctx, path)
		if err != nil {
			return nil, nil, err
		}
		replacements = append(replacements, replacement)
	}
	return lookupBounded(ctx, dir, stopDir, replacements, targets...)
}

// PathsReadingCreatedFile identifies lexical paths whose reads pass through
// the directory entry replaced by renaming a regular file over createdPath.
// Parent symlinks and links pointing to that entry are followed; a symlink at
// createdPath itself is replaced, so its old referent is not a written alias.
func PathsReadingCreatedFile(ctx context.Context, createdPath string, paths []string) ([]string, error) {
	result, err := PathsReadingCreatedFiles(ctx, []string{createdPath}, paths)
	if err != nil {
		return nil, err
	}
	return result[createdPath], nil
}

// PathsReadingCreatedFiles maps paths to the first replacement entry their
// reads encounter. Replacing a leaf symlink prevents a later read from reaching
// its old target, even when that target is another authored replacement.
func PathsReadingCreatedFiles(ctx context.Context, createdPaths, paths []string) (map[string][]string, error) {
	replacements := make([]createdFile, 0, len(createdPaths))
	for _, path := range createdPaths {
		replacement, err := newCreatedFile(ctx, path)
		if err != nil {
			return nil, err
		}
		replacements = append(replacements, replacement)
	}
	result := make(map[string][]string, len(createdPaths))
	for _, path := range paths {
		matched, err := firstCreatedFile(ctx, path, replacements)
		if err != nil {
			return nil, err
		}
		if matched >= 0 {
			for index, replacement := range replacements {
				if replacement.entry == replacements[matched].entry && !slices.Contains(result[createdPaths[index]], path) {
					result[createdPaths[index]] = append(result[createdPaths[index]], path)
				}
			}
		}
	}
	return result, ctx.Err()
}

type createdFile struct{ entry string }

func newCreatedFile(ctx context.Context, path string) (createdFile, error) {
	entry, err := replacementEntry(ctx, path)
	return createdFile{entry: entry}, err
}

// replacementEntry resolves the parent, never the leaf being renamed over.
// Missing parent directories are permitted: the scoped writer creates those
// directories before staging. Existing dangling parent links remain errors.
func replacementEntry(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("file replacement requires an absolute path")
	}
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	tail := []string{filepath.Base(path)}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			slices.Reverse(tail)
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, statErr := os.Lstat(parent); !errors.Is(statErr, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		tail = append(tail, filepath.Base(parent))
		parent = next
	}
}

func (f createdFile) readThrough(ctx context.Context, path string) (bool, error) {
	matched, err := firstCreatedFile(ctx, path, []createdFile{f})
	return matched >= 0, err
}

func firstCreatedFile(ctx context.Context, path string, replacements []createdFile) (int, error) {
	// Resolve leaf link chains only until the replacement entry. Following its
	// existing leaf symlink would incorrectly authorize the old referent.
	for range 255 {
		entry, err := replacementEntry(ctx, path)
		if err != nil {
			if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
				return -1, nil
			}
			return -1, err
		}
		for index, replacement := range replacements {
			if entry == replacement.entry {
				return index, nil
			}
		}
		info, err := os.Lstat(entry)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return -1, nil
		}
		if err != nil {
			return -1, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return -1, nil
		}
		link, err := os.Readlink(entry)
		if err != nil {
			return -1, err
		}
		path = link
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(entry), path)
		}
	}
	return -1, errors.New("file replacement alias has too many symbolic links")
}
