package imagegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type inputFileBinding struct {
	root     *os.Root
	parent   os.FileInfo
	identity os.FileInfo
}

type inputCapture struct {
	files map[string]inputFileBinding
	once  sync.Once
}

func (capture *inputCapture) close() {
	capture.once.Do(func() {
		closeInputFiles(capture.files)
	})
}

func captureInputFiles(paths []string) (*inputCapture, error) {
	capture := &inputCapture{files: make(map[string]inputFileBinding, len(paths))}
	for _, path := range paths {
		if _, exists := capture.files[path]; exists {
			continue
		}
		root, err := os.OpenRoot(filepath.Dir(path))
		if err != nil {
			capture.close()
			return nil, fmt.Errorf("capture input image directory %q: %w", filepath.Dir(path), err)
		}
		parent, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			capture.close()
			return nil, fmt.Errorf("inspect input image directory %q: %w", filepath.Dir(path), err)
		}
		identity, err := root.Stat(filepath.Base(path))
		if err != nil {
			_ = root.Close()
			capture.close()
			return nil, fmt.Errorf("inspect input image %q: %w", path, err)
		}
		if err := validateInputFile(path, identity); err != nil {
			_ = root.Close()
			capture.close()
			return nil, err
		}
		capture.files[path] = inputFileBinding{root: root, parent: parent, identity: identity}
	}
	return capture, nil
}

func (capture *inputCapture) validate() error {
	return validateInputFiles(capture.files)
}

func (capture *inputCapture) materialize() (map[string]inputFileBinding, error) {
	if err := capture.validate(); err != nil {
		return nil, err
	}
	files := make(map[string]inputFileBinding, len(capture.files))
	for path, binding := range capture.files {
		root, err := binding.root.OpenRoot(".")
		if err != nil {
			closeInputFiles(files)
			return nil, fmt.Errorf("retain input image directory %q: %w", filepath.Dir(path), err)
		}
		files[path] = inputFileBinding{root: root, parent: binding.parent, identity: binding.identity}
	}
	return files, nil
}

func validateInputFiles(files map[string]inputFileBinding) error {
	for path, binding := range files {
		current, err := os.OpenRoot(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("reopen input image directory %q: %w", filepath.Dir(path), err)
		}
		currentParent, parentErr := current.Stat(".")
		currentIdentity, identityErr := current.Stat(filepath.Base(path))
		closeErr := current.Close()
		if parentErr != nil || !os.SameFile(binding.parent, currentParent) {
			return fmt.Errorf("input image %q changed after capture; submit a new request", path)
		}
		if identityErr != nil || !os.SameFile(binding.identity, currentIdentity) {
			return fmt.Errorf("input image %q changed after capture; submit a new request", path)
		}
		if closeErr != nil {
			return fmt.Errorf("close input image directory %q: %w", filepath.Dir(path), closeErr)
		}
		retainedIdentity, err := binding.root.Stat(filepath.Base(path))
		if err != nil || !os.SameFile(binding.identity, retainedIdentity) {
			return fmt.Errorf("input image %q changed after capture; submit a new request", path)
		}
		if err := validateInputFile(path, retainedIdentity); err != nil {
			return err
		}
	}
	return nil
}

func validateInputFile(path string, info os.FileInfo) error {
	if info == nil {
		return errors.New("input image identity is unavailable")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input image is not a regular file: %s", path)
	}
	if info.Size() > maxInputImageBytes {
		return fmt.Errorf("input image %q exceeds %d bytes", path, maxInputImageBytes)
	}
	return nil
}

func closeInputFiles(files map[string]inputFileBinding) {
	for _, binding := range files {
		_ = binding.root.Close()
	}
}
