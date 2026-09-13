package trafficcapture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

type outputBinding struct {
	path             string
	ancestor         string
	root             *os.Root
	ancestorIdentity os.FileInfo
	missing          []string
}

func captureOutput(path string) (*outputBinding, error) {
	ancestor := filepath.Dir(path)
	var missing []string
	for {
		root, err := os.OpenRoot(ancestor)
		if err == nil {
			identity, statErr := root.Stat(".")
			if statErr != nil {
				_ = root.Close()
				return nil, fmt.Errorf("inspect capture output ancestor %q: %w", ancestor, statErr)
			}
			slices.Reverse(missing)
			if len(missing) == 0 {
				if _, leafErr := root.Lstat(filepath.Base(path)); leafErr == nil {
					_ = root.Close()
					return nil, fmt.Errorf("capture file already exists: %s", path)
				} else if !errors.Is(leafErr, os.ErrNotExist) {
					_ = root.Close()
					return nil, fmt.Errorf("access capture path: %w", leafErr)
				}
			}
			return &outputBinding{
				path:             path,
				ancestor:         ancestor,
				root:             root,
				ancestorIdentity: identity,
				missing:          missing,
			}, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return nil, fmt.Errorf("capture output directory %q: %w", filepath.Dir(path), err)
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
}

func (binding *outputBinding) close() {
	if binding != nil && binding.root != nil {
		_ = binding.root.Close()
	}
}

func (binding *outputBinding) create() (pathIdentity, error) {
	current, err := os.OpenRoot(binding.ancestor)
	if err != nil {
		return pathIdentity{}, fmt.Errorf("reopen capture output ancestor %q: %w", binding.ancestor, err)
	}
	currentIdentity, statErr := current.Stat(".")
	closeErr := current.Close()
	if statErr != nil || !os.SameFile(binding.ancestorIdentity, currentIdentity) {
		return pathIdentity{}, fmt.Errorf("capture output %q changed after approval; submit a new request", binding.path)
	}
	if closeErr != nil {
		return pathIdentity{}, fmt.Errorf("close capture output ancestor %q: %w", binding.ancestor, closeErr)
	}

	root, err := binding.root.OpenRoot(".")
	if err != nil {
		return pathIdentity{}, err
	}
	defer func() {
		_ = root.Close()
	}()
	for _, component := range binding.missing {
		_, err := root.Lstat(component)
		if err == nil {
			return pathIdentity{}, fmt.Errorf("capture output %q changed after approval; submit a new request", binding.path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return pathIdentity{}, err
		}
		if err = root.Mkdir(component, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				return pathIdentity{}, fmt.Errorf("capture output %q changed after approval; submit a new request", binding.path)
			}
			return pathIdentity{}, err
		}
		info, err := root.Lstat(component)
		if err != nil {
			return pathIdentity{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return pathIdentity{}, fmt.Errorf("capture output component %q is not a directory", component)
		}
		next, err := root.OpenRoot(component)
		if err != nil {
			return pathIdentity{}, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			_ = next.Close()
			return pathIdentity{}, errors.New("capture output directory changed while opening")
		}
		_ = root.Close()
		root = next
	}

	name := filepath.Base(binding.path)
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return pathIdentity{}, fmt.Errorf("capture file already exists: %s", binding.path)
		}
		return pathIdentity{}, err
	}
	info, err := file.Stat()
	closeErr = file.Close()
	if err != nil {
		return pathIdentity{}, err
	}
	if closeErr != nil {
		return pathIdentity{}, closeErr
	}
	return identityFromFileInfo(info)
}

func capturePathIdentity(path string, expectedMode os.FileMode) (pathIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return pathIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pathIdentity{}, err
	}
	if expectedMode.IsDir() {
		if !info.IsDir() {
			return pathIdentity{}, fmt.Errorf("unexpected filesystem object type: %s", path)
		}
	} else if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return pathIdentity{}, fmt.Errorf("target is not executable: %s", path)
	}
	return identityFromFileInfo(info)
}
