//go:build windows

package providerplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func snapshotDirectory(source, destination string) (snapshotResult, error) {
	if err := rejectWindowsReparse(source); err != nil {
		return snapshotResult{}, fmt.Errorf("open plugin source root: %w", err)
	}
	rootInfo, err := os.Lstat(source)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return snapshotResult{}, errors.New("plugin source root is not a safe directory")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return snapshotResult{}, fmt.Errorf("create snapshot root: %w", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return snapshotResult{}, fmt.Errorf("protect snapshot root: %w", err)
	}
	result := snapshotResult{DirectoryCount: 1}
	seen := map[string]string{}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("read plugin source tree")
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return errors.New("resolve plugin source path")
		}
		relative = filepath.ToSlash(relative)
		depth := strings.Count(relative, "/") + 1
		if !validRelativePath(relative, depth) || !validEntryName(entry.Name()) {
			return fmt.Errorf("plugin bundle path %q exceeds host limits", relative)
		}
		folded := strings.ToLower(relative)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("plugin bundle paths %q and %q collide case-insensitively", prior, relative)
		}
		seen[folded] = relative
		if err := rejectWindowsReparse(path); err != nil {
			return fmt.Errorf("plugin entry %q is a reparse point", relative)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin entry %q is unsafe", relative)
		}
		destinationPath := filepath.Join(destination, filepath.FromSlash(relative))
		if info.IsDir() {
			if result.DirectoryCount >= MaxBundleDirectories {
				return fmt.Errorf("plugin bundle exceeds %d directories", MaxBundleDirectories)
			}
			result.DirectoryCount++
			return os.Mkdir(destinationPath, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("plugin entry %q is not a regular file or directory", relative)
		}
		if result.FileCount >= MaxBundleFiles || info.Size() < 0 || info.Size() > MaxFileBytes || result.TotalBytes+info.Size() > MaxBundleBytes {
			return fmt.Errorf("plugin entry %q exceeds bundle limits", relative)
		}
		file, err := copyWindowsSourceFile(path, destinationPath, relative, info)
		if err != nil {
			return err
		}
		result.Files = append(result.Files, file)
		result.FileCount++
		result.TotalBytes += info.Size()
		return nil
	})
	if err != nil {
		return snapshotResult{}, err
	}
	result.Digest = canonicalBundleDigest(result.Files)
	return result, nil
}

func copyWindowsSourceFile(sourcePath, destinationPath, relative string, before os.FileInfo) (bundleFile, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return bundleFile{}, fmt.Errorf("open plugin entry %q", relative)
	}
	defer source.Close()
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(source.Fd()), &handleInfo); err != nil || handleInfo.NumberOfLinks != 1 || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return bundleFile{}, fmt.Errorf("plugin entry %q is unsafe or hard-linked", relative)
	}
	mode := normalizedFileMode(before.Mode())
	output, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return bundleFile{}, fmt.Errorf("create snapshot file %q", relative)
	}
	remove := true
	defer func() {
		output.Close()
		if remove {
			_ = os.Remove(destinationPath)
		}
	}()
	firstHash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, firstHash), io.LimitReader(source, MaxFileBytes+1))
	if err != nil || written != before.Size() {
		return bundleFile{}, fmt.Errorf("plugin entry %q changed during snapshot", relative)
	}
	if err := output.Chmod(mode); err != nil {
		return bundleFile{}, fmt.Errorf("protect snapshot file %q", relative)
	}
	if err := output.Sync(); err != nil {
		return bundleFile{}, fmt.Errorf("sync snapshot file %q", relative)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return bundleFile{}, fmt.Errorf("rewind plugin entry %q", relative)
	}
	secondHash := sha256.New()
	verified, err := io.Copy(secondHash, io.LimitReader(source, MaxFileBytes+1))
	if err != nil {
		return bundleFile{}, fmt.Errorf("verify plugin entry %q", relative)
	}
	after, err := source.Stat()
	if err != nil || verified != written || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || hex.EncodeToString(firstHash.Sum(nil)) != hex.EncodeToString(secondHash.Sum(nil)) {
		return bundleFile{}, fmt.Errorf("plugin entry %q changed during snapshot", relative)
	}
	if err := output.Close(); err != nil {
		return bundleFile{}, fmt.Errorf("close snapshot file %q", relative)
	}
	remove = false
	return bundleFile{Path: relative, Size: written, Mode: mode, SHA256: hex.EncodeToString(firstHash.Sum(nil))}, nil
}

func rejectWindowsReparse(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse points are forbidden")
	}
	return nil
}
