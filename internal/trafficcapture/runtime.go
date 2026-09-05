package trafficcapture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const embeddedRuntimeVersion = "mitmproxy-12.2.3-python-3.12.14"

func EmbeddedRuntimeAvailable() bool {
	return len(embeddedRuntimeArchive) != 0 && embeddedRuntimeTarget != "" && embeddedRuntimeLibrary != ""
}

func EmbeddedRuntimeError() error {
	if EmbeddedRuntimeAvailable() {
		return nil
	}
	return errors.New("traffic capture was not compiled with the embedded mitmproxy runtime; rebuild with `./build.sh --build --embedded-mitmproxy`")
}

func embeddedRuntimeUnavailableError() error {
	return EmbeddedRuntimeError()
}

func materializeEmbeddedRuntime() (string, error) {
	if !EmbeddedRuntimeAvailable() {
		return "", embeddedRuntimeUnavailableError()
	}
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(embeddedRuntimeArchive))
	parent := filepath.Join(cacheDirectory, "crux", "runtime", embeddedRuntimeVersion, embeddedRuntimeTarget)
	destination := filepath.Join(parent, digest)
	if validRuntimeDirectory(destination, digest) {
		return destination, nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create embedded runtime cache: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return "", fmt.Errorf("secure embedded runtime cache: %w", err)
	}
	if err := os.RemoveAll(destination); err != nil {
		return "", fmt.Errorf("replace invalid embedded runtime: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".extract-*")
	if err != nil {
		return "", fmt.Errorf("create embedded runtime staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return "", fmt.Errorf("secure embedded runtime staging directory: %w", err)
	}
	if err := extractRuntimeArchive(temporary); err != nil {
		return "", err
	}
	marker := filepath.Join(temporary, ".crux-runtime-sha256")
	if err := os.WriteFile(marker, []byte(digest+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write embedded runtime marker: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		if validRuntimeDirectory(destination, digest) {
			return destination, nil
		}
		return "", fmt.Errorf("install embedded runtime: %w", err)
	}
	if !validRuntimeDirectory(destination, digest) {
		return "", errors.New("installed embedded runtime failed validation")
	}
	return destination, nil
}

func validRuntimeDirectory(path, digest string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	marker, err := os.ReadFile(filepath.Join(path, ".crux-runtime-sha256"))
	if err != nil || strings.TrimSpace(string(marker)) != digest {
		return false
	}
	library, err := os.Stat(filepath.Join(path, embeddedRuntimeLibrary))
	return err == nil && library.Mode().IsRegular()
}

func extractRuntimeArchive(destination string) error {
	compressed, err := gzip.NewReader(bytes.NewReader(embeddedRuntimeArchive))
	if err != nil {
		return fmt.Errorf("open embedded runtime archive: %w", err)
	}
	defer compressed.Close()
	return extractRuntimeTar(destination, tar.NewReader(compressed))
}

func extractRuntimeTar(destination string, archive *tar.Reader) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open embedded runtime destination: %w", err)
	}
	defer root.Close()
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read embedded runtime archive: %w", err)
		}
		path, err := runtimeArchivePath(destination, header.Name)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		parent := filepath.Dir(name)
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create embedded runtime parent: %w", err)
		}
		for current := parent; current != "."; current = filepath.Dir(current) {
			info, err := root.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("unsafe embedded runtime symlink parent %q", current)
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return fmt.Errorf("create embedded runtime directory: %w", err)
			}
		case tar.TypeReg:
			file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("create embedded runtime file: %w", err)
			}
			_, copyErr := io.Copy(file, archive)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract embedded runtime file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close embedded runtime file: %w", closeErr)
			}
		case tar.TypeSymlink:
			target, err := runtimeArchiveLink(destination, filepath.Dir(path), header.Linkname)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(filepath.Dir(path), target)
			if err != nil {
				return fmt.Errorf("resolve embedded runtime link: %w", err)
			}
			if err := root.Symlink(relative, name); err != nil {
				return fmt.Errorf("create embedded runtime link: %w", err)
			}
		case tar.TypeLink:
			target, err := runtimeArchivePath(destination, header.Linkname)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(destination, target)
			if err != nil {
				return err
			}
			if err := root.Link(relative, name); err != nil {
				return fmt.Errorf("create embedded runtime hard link: %w", err)
			}
		default:
			return fmt.Errorf("unsupported embedded runtime archive entry %q", header.Name)
		}
	}
}

func runtimeArchivePath(root, name string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." || !filepath.IsLocal(cleaned) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe embedded runtime archive path %q", name)
	}
	path := filepath.Join(root, cleaned)
	if !pathWithin(root, path) {
		return "", fmt.Errorf("unsafe embedded runtime archive path %q", name)
	}
	return path, nil
}

func runtimeArchiveLink(root, parent, name string) (string, error) {
	if strings.HasPrefix(name, "/") || filepath.IsAbs(filepath.FromSlash(name)) || filepath.VolumeName(filepath.FromSlash(name)) != "" {
		return "", fmt.Errorf("unsafe embedded runtime archive link %q", name)
	}
	path := filepath.Clean(filepath.Join(parent, filepath.FromSlash(name)))
	if !pathWithin(root, path) {
		return "", fmt.Errorf("unsafe embedded runtime archive link %q", name)
	}
	return path, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
