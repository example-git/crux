package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/oauth/accounts"
)

var errAuthenticationInputsChanged = errors.New("authentication configuration inputs changed during capture")

// authenticationConfigInputs retains raw inputs, not evaluated shell output.
// order preserves merge priority; files also retain previously loaded inputs
// and writable targets that are currently missing. Never serialize this value.
type authenticationConfigInputs struct {
	valid bool
	order []string
	files []authenticationInputFile
}

type authenticationInputFile struct {
	path string
	data []byte
	info authenticationInputFileInfo
}

type authenticationInputFileInfo struct {
	exists   bool
	identity [sha256.Size]byte
	size     int64
	mode     os.FileMode
	modified int64
}

func (authenticationInputFile) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication configuration preimages are private")
}
func (authenticationInputFile) String() string {
	return "[private authentication configuration preimage]"
}
func (authenticationInputFile) GoString() string {
	return "[private authentication configuration preimage]"
}
func (authenticationInputFile) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication configuration preimage]"))
}

func (authenticationConfigInputs) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication configuration inputs are private")
}
func (authenticationConfigInputs) String() string {
	return "[private authentication configuration inputs]"
}
func (authenticationConfigInputs) GoString() string {
	return "[private authentication configuration inputs]"
}
func (authenticationConfigInputs) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication configuration inputs]"))
}

func (a authenticationConfigInputs) sameObservation(b authenticationConfigInputs) bool {
	if !a.valid || !b.valid || !slices.Equal(a.order, b.order) || len(a.files) != len(b.files) {
		return false
	}
	for i, file := range a.files {
		other := b.files[i]
		if file.path != other.path || file.info != other.info || !bytes.Equal(file.data, other.data) {
			return false
		}
	}
	return true
}

// file returns an independent raw preimage for later fixed scoped mutations.
// Missing captured files return found=true and info.exists=false.
func (a authenticationConfigInputs) file(path string) (authenticationInputFile, bool) {
	path = filepath.Clean(path)
	for _, file := range a.files {
		if file.path == path {
			file.data = bytes.Clone(file.data)
			return file, true
		}
	}
	return authenticationInputFile{}, false
}

// captureAuthenticationInputsLocked requires writeMu for reading or writing.
// It never resolves values, executes shell configuration, publishes a runtime,
// creates a config lock, or falls back to live process environment variables.
func (s *ConfigStore) captureAuthenticationInputsLocked(ctx context.Context, snapshot RuntimeSnapshot, workspacePaths ...string) (authenticationConfigInputs, error) {
	if snapshot.IsClientOwned() {
		return authenticationConfigInputs{}, ErrClientRuntimeManaged
	}
	order, paths, err := s.authenticationInputPathsLocked(ctx, snapshot, workspacePaths...)
	if err != nil {
		return authenticationConfigInputs{}, authenticationInputError(err)
	}
	result := authenticationConfigInputs{valid: true, order: order, files: make([]authenticationInputFile, 0, len(paths))}
	for _, path := range paths {
		file, err := readAuthenticationInput(ctx, path)
		if err != nil {
			return authenticationConfigInputs{}, authenticationInputError(err)
		}
		result.files = append(result.files, file)
	}
	finalOrder, finalPaths, err := s.authenticationInputPathsLocked(ctx, snapshot, workspacePaths...)
	if err != nil {
		return authenticationConfigInputs{}, authenticationInputError(err)
	}
	if !slices.Equal(order, finalOrder) || !slices.Equal(paths, finalPaths) {
		return authenticationConfigInputs{}, errAuthenticationInputsChanged
	}
	return result, nil
}

func (s *ConfigStore) authenticationInputPathsLocked(ctx context.Context, snapshot RuntimeSnapshot, workspacePaths ...string) ([]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	workspacePath := s.workspacePath
	if len(workspacePaths) > 0 {
		workspacePath = workspacePaths[0]
	}
	var order []string
	if s.workingDir != "" {
		if !filepath.IsAbs(s.workingDir) {
			return nil, nil, errors.New("authentication working directory is not absolute")
		}
		environment := s.baseEnvironment
		if environment == nil {
			environment = snapshot.environment
		}
		globals, err := authenticationGlobalInputPaths(environment)
		if err != nil {
			return nil, nil, err
		}
		order = append(order, globals...)
		// Real loading populated this cache before publishing the store. Do not
		// run git (or consult live PATH/environment) merely to show auth status.
		boundary := s.workingDir
		if cached, ok := worktreeRootCache.Load(s.workingDir); ok && cached.(string) != "" {
			boundary = cached.(string)
		}
		found, err := fsext.LookupBounded(s.workingDir, boundary, "."+appName+"rc", appName+"rc", "."+appName+".json", appName+".json")
		if err != nil {
			return nil, nil, err
		}
		slices.Reverse(found)
		order = append(order, found...)
	}
	if workspacePath != "" {
		order = append(order, workspacePath)
	}
	// Pathless test/manual stores have no implicit host-global discovery. Their
	// explicit loaded inputs and persistence targets are still observed.
	paths := append(slices.Clone(order), s.loadedPaths...)
	paths = append(paths, s.globalDataPath, workspacePath)
	clean := func(values []string, unique bool) ([]string, error) {
		result := make([]string, 0, len(values))
		seen := map[string]bool{}
		for _, path := range values {
			if path == "" {
				continue
			}
			if !filepath.IsAbs(path) {
				return nil, errors.New("authentication configuration path is not absolute")
			}
			path = filepath.Clean(path)
			if unique && seen[path] {
				continue
			}
			seen[path] = true
			result = append(result, path)
		}
		return result, nil
	}
	var err error
	order, err = clean(order, false)
	if err != nil {
		return nil, nil, err
	}
	paths, err = clean(paths, true)
	if err != nil {
		return nil, nil, err
	}
	return order, paths, ctx.Err()
}

// Derive only from captured values. The ordinary loading helpers have legacy
// live-home fallback behavior, which must not route a status read elsewhere.
func authenticationGlobalInputPaths(environment env.Env) ([]string, error) {
	get := func(key string) string {
		if environment == nil {
			return ""
		}
		return environment.Get(key)
	}
	home := get(authenticationHomeVariable())
	configDir := get("CRUX_GLOBAL_CONFIG")
	if configDir == "" {
		if xdg := get("XDG_CONFIG_HOME"); xdg != "" {
			configDir = filepath.Join(xdg, appName)
		} else if filepath.IsAbs(home) {
			configDir = filepath.Join(home, ".ai-cli", appName)
		}
	}
	dataDir := get("CRUX_GLOBAL_DATA")
	if dataDir == "" {
		if xdg := get("XDG_DATA_HOME"); xdg != "" {
			dataDir = filepath.Join(xdg, appName)
		} else if runtime.GOOS == "windows" {
			if local := get("LOCALAPPDATA"); local != "" {
				dataDir = filepath.Join(local, appName)
			} else if filepath.IsAbs(home) {
				dataDir = filepath.Join(home, "AppData", "Local", appName)
			}
		} else if filepath.IsAbs(home) {
			dataDir = filepath.Join(home, ".ai-cli", "data", appName)
		}
	}
	if !filepath.IsAbs(configDir) || !filepath.IsAbs(dataDir) {
		return nil, errors.New("captured authentication config directories are unavailable or not absolute")
	}
	global := filepath.Join(configDir, appName+".json")
	return []string{systemConfigPath, global, shellConfigSibling(global), filepath.Join(dataDir, appName+".json")}, nil
}

func readAuthenticationInput(ctx context.Context, path string) (authenticationInputFile, error) {
	if err := ctx.Err(); err != nil {
		return authenticationInputFile{}, err
	}
	result := authenticationInputFile{path: path}
	file, err := openAuthenticationInput(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return authenticationInputFile{}, errAuthenticationInputsChanged
		}
		return result, ctx.Err()
	}
	if err != nil {
		return authenticationInputFile{}, err
	}
	defer file.Close()
	result.info, err = observeAuthenticationInput(file)
	if err != nil {
		return authenticationInputFile{}, err
	}
	result.data, err = io.ReadAll(authenticationInputReader{ctx: ctx, reader: file})
	if err != nil {
		return authenticationInputFile{}, err
	}
	if err := verifyAuthenticationInput(ctx, path, file, result.info); err != nil {
		return authenticationInputFile{}, err
	}
	return result, nil
}

func observeAuthenticationInput(file *os.File) (authenticationInputFileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return authenticationInputFileInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return authenticationInputFileInfo{}, errors.New("authentication configuration input is not a regular file")
	}
	identity, err := authenticationInputIdentity(file, info)
	if err != nil {
		return authenticationInputFileInfo{}, err
	}
	return authenticationInputFileInfo{exists: true, identity: identity, size: info.Size(), mode: info.Mode(), modified: info.ModTime().UnixNano()}, nil
}

func verifyAuthenticationInput(ctx context.Context, path string, file *os.File, expected authenticationInputFileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := observeAuthenticationInput(file)
	if err != nil {
		return err
	}
	if current != expected {
		return errAuthenticationInputsChanged
	}
	target, err := openAuthenticationInput(path)
	if err != nil {
		return err
	}
	defer target.Close()
	actual, err := observeAuthenticationInput(target)
	if err != nil {
		return err
	}
	if actual != expected {
		return errAuthenticationInputsChanged
	}
	return ctx.Err()
}

type authenticationInputReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r authenticationInputReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func authenticationInputError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errAuthenticationInputsChanged) {
		return err
	}
	return errors.New("authentication configuration inputs cannot be read")
}

// Bracket input reads with full account observations, then compare the final
// raw input read. This detects observed interleaving without executing config
// or introducing lock files beside read-only system/project inputs.
func validateAuthenticationObservations(first, second accounts.Snapshot, inputs, finalInputs authenticationConfigInputs) error {
	if !first.SameObservation(second) {
		return errors.New("authentication account store changed during capture")
	}
	if !inputs.sameObservation(finalInputs) {
		return errAuthenticationInputsChanged
	}
	return nil
}
