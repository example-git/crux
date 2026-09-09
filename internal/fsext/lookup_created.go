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
	replacement, err := newCreatedFile(ctx, createdPath)
	if err != nil {
		return nil, nil, err
	}
	return lookupBounded(ctx, dir, stopDir, &replacement, targets...)
}

// PathsReadingCreatedFile identifies lexical paths whose reads pass through
// the directory entry replaced by renaming a regular file over createdPath.
// Parent symlinks and links pointing to that entry are followed; a symlink at
// createdPath itself is replaced, so its old referent is not a written alias.
func PathsReadingCreatedFile(ctx context.Context, createdPath string, paths []string) ([]string, error) {
	replacement, err := newCreatedFile(ctx, createdPath)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, path := range paths {
		matches, err := replacement.readThrough(ctx, path)
		if err != nil {
			return nil, err
		}
		if matches && !slices.Contains(result, path) {
			result = append(result, path)
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
	// Resolve leaf link chains only until the replacement entry. Following its
	// existing leaf symlink would incorrectly authorize the old referent.
	for range 255 {
		entry, err := replacementEntry(ctx, path)
		if err != nil {
			if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		if entry == f.entry {
			return true, nil
		}
		info, err := os.Lstat(entry)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		link, err := os.Readlink(entry)
		if err != nil {
			return false, err
		}
		path = link
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(entry), path)
		}
	}
	return false, errors.New("file replacement alias has too many symbolic links")
}
