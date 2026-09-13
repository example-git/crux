package imagegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type outputDirectoryCapture struct {
	root    *os.Root
	missing []string
}

type outputCapture struct {
	directories map[string]outputDirectoryCapture
	once        sync.Once
}

func (capture *outputCapture) close() {
	capture.once.Do(func() {
		for _, directory := range capture.directories {
			_ = directory.root.Close()
		}
	})
}

func (request JobRequest) CaptureOutputDirectories() (JobRequest, func(), error) {
	return request.captureOutputDirectories("")
}

func (request JobRequest) captureOutputDirectories(numberedDirectory string) (JobRequest, func(), error) {
	if request.outputCapture != nil {
		for _, path := range request.OutputPaths {
			if _, ok := request.outputCapture.directories[filepath.Dir(path)]; !ok {
				return request, func() {}, errors.New("image output directory was not captured")
			}
		}
		return request, func() {}, nil
	}
	capture := &outputCapture{directories: make(map[string]outputDirectoryCapture)}
	directories := []string{}
	for _, path := range request.OutputPaths {
		directories = append(directories, filepath.Dir(path))
	}
	if numberedDirectory != "" {
		directories = append(directories, numberedDirectory)
	}
	for _, directory := range directories {
		if _, ok := capture.directories[directory]; ok {
			continue
		}
		if !filepath.IsAbs(directory) {
			capture.close()
			return request, func() {}, errors.New("image output directory must be absolute")
		}
		ancestor := directory
		var missing []string
		for {
			root, err := os.OpenRoot(ancestor)
			if err == nil {
				slices.Reverse(missing)
				capture.directories[directory] = outputDirectoryCapture{root: root, missing: missing}
				break
			}
			if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
				capture.close()
				return request, func() {}, fmt.Errorf("capture image output directory %q: %w", directory, err)
			}
			missing = append(missing, filepath.Base(ancestor))
			ancestor = filepath.Dir(ancestor)
		}
	}
	request.outputCapture = capture
	return request, capture.close, nil
}

func (capture *outputCapture) openDirectory(directory string, create bool) (*os.Root, error) {
	binding, ok := capture.directories[directory]
	if !ok {
		return nil, errors.New("image output directory was not captured")
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
			return nil, fmt.Errorf("image output component %q is not a directory", component)
		}
		next, err := root.OpenRoot(component)
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			_ = next.Close()
			return nil, errors.New("image output directory changed while opening")
		}
		root = next
	}
	return root, nil
}

func (capture *outputCapture) materialize() (map[string]*os.Root, error) {
	roots := make(map[string]*os.Root)
	for directory := range capture.directories {
		root, err := capture.openDirectory(directory, true)
		if err != nil {
			closeOutputRoots(roots)
			return nil, fmt.Errorf("prepare image output directory %q: %w", directory, err)
		}
		roots[directory] = root
	}
	return roots, nil
}

func (request JobRequest) numberedOutputPaths(directory string, reserved map[string]string) ([]string, error) {
	root := request.outputRoots[directory]
	if root == nil {
		var release func()
		var err error
		request, release, err = request.captureOutputDirectories(directory)
		if err != nil {
			return nil, err
		}
		defer release()
		root, err = request.outputCapture.openDirectory(directory, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if root != nil {
			defer root.Close()
		}
	}
	inspect := func(name string) (os.FileInfo, error) {
		if root == nil {
			return nil, os.ErrNotExist
		}
		return root.Lstat(name)
	}
	return nextNumberedOutputPaths(directory, request.Count, request.Force, request.Backend, reserved, inspect, request.OutputExtension)
}

func closeOutputRoots(roots map[string]*os.Root) {
	for _, root := range roots {
		_ = root.Close()
	}
}
