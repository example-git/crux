//go:build !windows

package providerplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

func snapshotDirectory(source, destination string) (snapshotResult, error) {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return snapshotResult{}, fmt.Errorf("open plugin source root: %w", err)
	}
	root := os.NewFile(uintptr(fd), source)
	defer root.Close()
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return snapshotResult{}, fmt.Errorf("create snapshot root: %w", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return snapshotResult{}, fmt.Errorf("protect snapshot root: %w", err)
	}
	seen := map[string]string{}
	result := snapshotResult{DirectoryCount: 1}
	if err := copySourceDirectory(root, destination, "", 0, seen, &result); err != nil {
		return snapshotResult{}, err
	}
	result.Digest = canonicalBundleDigest(result.Files)
	return result, nil
}

func copySourceDirectory(source *os.File, destination, relative string, depth int, seen map[string]string, result *snapshotResult) error {
	remaining := MaxBundleFiles + MaxBundleDirectories - result.FileCount - result.DirectoryCount + 1
	entries, err := source.ReadDir(remaining)
	if err != nil && err != io.EOF {
		return fmt.Errorf("read plugin source directory %q: %w", relative, err)
	}
	if len(entries) >= remaining {
		return fmt.Errorf("plugin bundle exceeds %d files and %d directories", MaxBundleFiles, MaxBundleDirectories)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		name := entry.Name()
		if !validEntryName(name) {
			return fmt.Errorf("plugin bundle contains unsafe entry name %q", name)
		}
		rel := name
		if relative != "" {
			rel = relative + "/" + name
		}
		if !validRelativePath(rel, depth+1) {
			return fmt.Errorf("plugin bundle path %q exceeds host limits", rel)
		}
		folded := strings.ToLower(rel)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("plugin bundle paths %q and %q collide case-insensitively", prior, rel)
		}
		seen[folded] = rel

		fd, err := unix.Openat(int(source.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("securely open plugin entry %q: %w", rel, err)
		}
		child := os.NewFile(uintptr(fd), rel)
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			child.Close()
			return fmt.Errorf("inspect plugin entry %q: %w", rel, err)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if result.DirectoryCount >= MaxBundleDirectories {
				child.Close()
				return fmt.Errorf("plugin bundle exceeds %d directories", MaxBundleDirectories)
			}
			result.DirectoryCount++
			dest := filepath.Join(destination, filepath.FromSlash(rel))
			if err := os.Mkdir(dest, 0o700); err != nil {
				child.Close()
				return fmt.Errorf("create snapshot directory %q: %w", rel, err)
			}
			if err := copySourceDirectory(child, destination, rel, depth+1, seen, result); err != nil {
				child.Close()
				return err
			}
			if err := child.Close(); err != nil {
				return fmt.Errorf("close plugin source directory %q: %w", rel, err)
			}
		case unix.S_IFREG:
			if stat.Nlink != 1 {
				child.Close()
				return fmt.Errorf("plugin entry %q is hard-linked", rel)
			}
			if result.FileCount >= MaxBundleFiles {
				child.Close()
				return fmt.Errorf("plugin bundle exceeds %d files", MaxBundleFiles)
			}
			if stat.Size < 0 || stat.Size > MaxFileBytes || result.TotalBytes+stat.Size > MaxBundleBytes {
				child.Close()
				return fmt.Errorf("plugin entry %q exceeds bundle size limits", rel)
			}
			file, err := copySourceFile(child, destination, rel, stat)
			if closeErr := child.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			result.Files = append(result.Files, file)
			result.FileCount++
			result.TotalBytes += stat.Size
		default:
			child.Close()
			return fmt.Errorf("plugin entry %q is not a regular file or directory", rel)
		}
	}
	return nil
}

func copySourceFile(source *os.File, destination, relative string, before unix.Stat_t) (bundleFile, error) {
	beforeInfo, err := source.Stat()
	if err != nil {
		return bundleFile{}, fmt.Errorf("inspect plugin entry %q: %w", relative, err)
	}
	destinationPath := filepath.Join(destination, filepath.FromSlash(relative))
	mode := normalizedFileMode(os.FileMode(before.Mode))
	output, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return bundleFile{}, fmt.Errorf("create snapshot file %q: %w", relative, err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(destinationPath)
		}
	}()
	firstHash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, firstHash), io.LimitReader(source, MaxFileBytes+1))
	if err != nil {
		output.Close()
		return bundleFile{}, fmt.Errorf("copy plugin entry %q: %w", relative, err)
	}
	if written != before.Size {
		output.Close()
		return bundleFile{}, fmt.Errorf("plugin entry %q changed size during snapshot", relative)
	}
	if err := output.Chmod(mode); err != nil {
		output.Close()
		return bundleFile{}, fmt.Errorf("protect snapshot file %q: %w", relative, err)
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return bundleFile{}, fmt.Errorf("sync snapshot file %q: %w", relative, err)
	}
	if err := output.Close(); err != nil {
		return bundleFile{}, fmt.Errorf("close snapshot file %q: %w", relative, err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return bundleFile{}, fmt.Errorf("rewind plugin entry %q: %w", relative, err)
	}
	secondHash := sha256.New()
	verified, err := io.Copy(secondHash, io.LimitReader(source, MaxFileBytes+1))
	if err != nil {
		return bundleFile{}, fmt.Errorf("verify plugin entry %q: %w", relative, err)
	}
	afterInfo, err := source.Stat()
	if err != nil {
		return bundleFile{}, fmt.Errorf("reinspect plugin entry %q: %w", relative, err)
	}
	if verified != written || !os.SameFile(beforeInfo, afterInfo) || beforeInfo.Size() != afterInfo.Size() ||
		!beforeInfo.ModTime().Equal(afterInfo.ModTime()) || hex.EncodeToString(firstHash.Sum(nil)) != hex.EncodeToString(secondHash.Sum(nil)) {
		return bundleFile{}, fmt.Errorf("plugin entry %q changed during snapshot", relative)
	}
	remove = false
	return bundleFile{Path: relative, Size: written, Mode: mode, SHA256: hex.EncodeToString(firstHash.Sum(nil))}, nil
}
