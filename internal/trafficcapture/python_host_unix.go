//go:build darwin || linux

package trafficcapture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ebitengine/purego"
)

type pythonHost struct {
	decodeLocale   func(string, *uintptr) uintptr
	initialize     func(int32)
	isInitialized  func() int32
	runString      func(string, uintptr) int32
	finalize       func() int32
	setHome        func(uintptr)
	setPath        func(uintptr)
	setProgramName func(uintptr)
	rawFree        func(uintptr)
}

func runEmbeddedPython(source string) error {
	root, err := materializeEmbeddedRuntime()
	if err != nil {
		return err
	}
	libraryPath := filepath.Join(root, embeddedRuntimeLibrary)
	handle, err := purego.Dlopen(libraryPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load embedded CPython: %w", err)
	}
	host, err := loadPythonHost(handle)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	clearPythonEnvironment()
	home := host.decodeLocale(root, nil)
	if home == 0 {
		return fmt.Errorf("decode embedded Python home")
	}
	defer host.rawFree(home)
	paths := []string{
		filepath.Join(root, "lib", "python3.12"),
		filepath.Join(root, "lib", "python3.12", "lib-dynload"),
		filepath.Join(root, "lib", "python3.12", "site-packages"),
	}
	path := host.decodeLocale(strings.Join(paths, string(os.PathListSeparator)), nil)
	if path == 0 {
		return fmt.Errorf("decode embedded Python module path")
	}
	defer host.rawFree(path)
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate Crux executable: %w", err)
	}
	program := host.decodeLocale(executable, nil)
	if program == 0 {
		return fmt.Errorf("decode Crux executable path")
	}
	defer host.rawFree(program)
	host.setProgramName(program)
	host.setHome(home)
	host.setPath(path)
	host.initialize(0)
	if host.isInitialized() == 0 {
		return fmt.Errorf("initialize embedded CPython")
	}
	runCode := host.runString(source, 0)
	finalizeCode := host.finalize()
	if runCode != 0 {
		return fmt.Errorf("embedded mitmproxy worker exited with status %d", runCode)
	}
	if finalizeCode != 0 {
		return fmt.Errorf("finalize embedded CPython with status %d", finalizeCode)
	}
	return nil
}

func clearPythonEnvironment() {
	for _, value := range os.Environ() {
		name, _, ok := strings.Cut(value, "=")
		if ok && (strings.HasPrefix(name, "PYTHON") || name == "_PYTHON_SYSCONFIGDATA_NAME") {
			_ = os.Unsetenv(name)
		}
	}
}

func loadPythonHost(handle uintptr) (pythonHost, error) {
	host := pythonHost{}
	bindings := []struct {
		name   string
		target any
	}{
		{"Py_DecodeLocale", &host.decodeLocale},
		{"Py_InitializeEx", &host.initialize},
		{"Py_IsInitialized", &host.isInitialized},
		{"PyRun_SimpleStringFlags", &host.runString},
		{"Py_FinalizeEx", &host.finalize},
		{"Py_SetPythonHome", &host.setHome},
		{"Py_SetPath", &host.setPath},
		{"Py_SetProgramName", &host.setProgramName},
		{"PyMem_RawFree", &host.rawFree},
	}
	for _, binding := range bindings {
		symbol, err := purego.Dlsym(handle, binding.name)
		if err != nil {
			return pythonHost{}, fmt.Errorf("load embedded CPython symbol %s: %w", binding.name, err)
		}
		purego.RegisterFunc(binding.target, symbol)
	}
	return host, nil
}
