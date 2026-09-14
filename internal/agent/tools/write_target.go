package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

type writeTargetBinding struct {
	path             string
	ancestor         string
	root             *os.Root
	ancestorIdentity os.FileInfo
	missing          []string
	identity         os.FileInfo
}

func captureWriteTarget(path string) (*writeTargetBinding, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("write target must be absolute")
	}

	directory := filepath.Dir(path)
	ancestor := directory
	var missing []string
	for {
		root, err := os.OpenRoot(ancestor)
		if err == nil {
			ancestorIdentity, statErr := root.Stat(".")
			if statErr != nil {
				_ = root.Close()
				return nil, fmt.Errorf("inspect write target ancestor %q: %w", ancestor, statErr)
			}
			slices.Reverse(missing)
			binding := &writeTargetBinding{
				path:             path,
				ancestor:         ancestor,
				root:             root,
				ancestorIdentity: ancestorIdentity,
				missing:          missing,
			}
			if len(missing) == 0 {
				identity, identityErr := root.Stat(filepath.Base(path))
				if identityErr == nil {
					binding.identity = identity
				} else if !errors.Is(identityErr, os.ErrNotExist) {
					_ = root.Close()
					return nil, fmt.Errorf("inspect write target %q: %w", path, identityErr)
				}
			}
			return binding, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return nil, fmt.Errorf("capture write target directory %q: %w", directory, err)
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
}

func (binding *writeTargetBinding) close() {
	if binding != nil && binding.root != nil {
		_ = binding.root.Close()
	}
}

func (binding *writeTargetBinding) exists() bool {
	return binding != nil && binding.identity != nil
}

func (binding *writeTargetBinding) info() os.FileInfo {
	if binding == nil {
		return nil
	}
	return binding.identity
}

func (binding *writeTargetBinding) validateAncestor() error {
	current, err := os.OpenRoot(binding.ancestor)
	if err != nil {
		return fmt.Errorf("reopen write target ancestor %q: %w", binding.ancestor, err)
	}
	currentIdentity, statErr := current.Stat(".")
	closeErr := current.Close()
	if statErr != nil || !os.SameFile(binding.ancestorIdentity, currentIdentity) {
		return fmt.Errorf("write target %q changed after capture; submit a new request", binding.path)
	}
	if closeErr != nil {
		return fmt.Errorf("close write target ancestor %q: %w", binding.ancestor, closeErr)
	}
	return nil
}

func (binding *writeTargetBinding) openDirectory(create bool) (*os.Root, error) {
	if err := binding.validateAncestor(); err != nil {
		return nil, err
	}
	root, err := binding.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, component := range binding.missing {
		info, err := root.Lstat(component)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = root.Mkdir(component, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				_ = root.Close()
				return nil, err
			}
			info, err = root.Lstat(component)
		}
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = root.Close()
			return nil, fmt.Errorf("write target component %q is not a directory", component)
		}
		next, err := root.OpenRoot(component)
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			_ = next.Close()
			return nil, errors.New("write target directory changed while opening")
		}
		root = next
	}
	return root, nil
}

func (binding *writeTargetBinding) openExisting(flag int) (*os.File, os.FileInfo, error) {
	if !binding.exists() {
		return nil, nil, os.ErrNotExist
	}
	root, err := binding.openDirectory(false)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()

	name := filepath.Base(binding.path)
	current, err := root.Stat(name)
	if err != nil || !os.SameFile(binding.identity, current) {
		return nil, nil, fmt.Errorf("write target %q changed after capture; submit a new request", binding.path)
	}
	file, err := root.OpenFile(name, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(binding.identity, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("write target %q changed while opening; submit a new request", binding.path)
	}
	return file, opened, nil
}

func (binding *writeTargetBinding) read() ([]byte, error) {
	file, _, err := binding.openExisting(os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (binding *writeTargetBinding) openForUpdate() (*os.File, []byte, os.FileInfo, error) {
	file, info, err := binding.openExisting(os.O_RDWR)
	if err != nil {
		return nil, nil, nil, err
	}
	content, err := io.ReadAll(file)
	if err != nil {
		_ = file.Close()
		return nil, nil, nil, err
	}
	return file, content, info, nil
}

func writeOpenedFile(file *os.File, content []byte) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	_, err := file.Write(content)
	return err
}

func (binding *writeTargetBinding) create(content []byte) error {
	if binding.exists() {
		return os.ErrExist
	}
	root, err := binding.openDirectory(true)
	if err != nil {
		return err
	}
	defer root.Close()

	name := filepath.Base(binding.path)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
