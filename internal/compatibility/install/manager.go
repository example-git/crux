package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
)

const stateVersion = 1

var canonicalAliases = []string{"codex", "claude", "agy", "copilot"}

type AliasState string

const (
	AliasMissing   AliasState = "missing"
	AliasLinked    AliasState = "linked"
	AliasStale     AliasState = "stale"
	AliasCollision AliasState = "collision"
)

type AliasStatus struct {
	Name  string
	Path  string
	State AliasState
}

type Status struct {
	Root       string
	Bin        string
	Executable string
	Installed  bool
	Enabled    bool
	PathSetup  bool
	PathActive bool
	Profile    string
	Aliases    []AliasStatus
}

type InvocationMode struct {
	Managed bool
	Enabled bool
	Bin     string
	Path    string
}

type Options struct {
	Executable          string
	Shell               string
	Profile             string
	SkipPath            bool
	VerifyCompatibility bool
}

type Manager struct {
	root        string
	failForTest func(string) error
}

const (
	operationWritePath     = "write-path"
	operationUpdateProfile = "update-profile"
	operationSaveState     = "save-state"
)

type pathSetupPlan struct {
	shell       string
	profile     string
	pathFile    string
	pathContent string
	profileLine string
}

type fileSnapshot struct {
	path           string
	existed        bool
	mode           fs.FileMode
	modTime        time.Time
	data           []byte
	identityBackup string
	missingParents []string
}

type filesystemTransaction struct {
	snapshots []*fileSnapshot
	watched   map[string]struct{}
}

func (t *filesystemTransaction) watch(path string, preserveIdentity bool) error {
	if path == "" {
		return nil
	}
	path = filepath.Clean(path)
	if _, ok := t.watched[path]; ok {
		return nil
	}
	t.watched[path] = struct{}{}
	snapshot := &fileSnapshot{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		snapshot.missingParents = missingParentDirectories(filepath.Dir(path))
		t.snapshots = append(t.snapshots, snapshot)
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("cannot transactionally update non-regular file %q", path)
	}
	snapshot.existed = true
	snapshot.mode = info.Mode().Perm()
	snapshot.modTime = info.ModTime()
	if preserveIdentity {
		temporary, err := os.CreateTemp(filepath.Dir(path), ".crux-compatibility-backup-*")
		if err != nil {
			return fmt.Errorf("prepare rollback for %q: %w", path, err)
		}
		backup := temporary.Name()
		if closeErr := temporary.Close(); closeErr != nil {
			_ = os.Remove(backup)
			return fmt.Errorf("prepare rollback for %q: %w", path, closeErr)
		}
		if err := os.Remove(backup); err != nil {
			return fmt.Errorf("prepare rollback for %q: %w", path, err)
		}
		if err := os.Link(path, backup); err != nil {
			return fmt.Errorf("prepare rollback for %q: %w", path, err)
		}
		snapshot.identityBackup = backup
	} else {
		snapshot.data, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("snapshot %q: %w", path, err)
		}
	}
	t.snapshots = append(t.snapshots, snapshot)
	return nil
}

func (t *filesystemTransaction) fail(operationErr error) error {
	if rollbackErr := t.rollback(); rollbackErr != nil {
		return errors.Join(operationErr, fmt.Errorf("rollback compatibility transaction: %w", rollbackErr))
	}
	return operationErr
}

func (t *filesystemTransaction) rollback() error {
	var rollbackErrors []error
	for index := len(t.snapshots) - 1; index >= 0; index-- {
		snapshot := t.snapshots[index]
		if !snapshot.existed {
			if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("remove new file %q: %w", snapshot.path, err))
			}
			for _, directory := range snapshot.missingParents {
				if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
					if !errors.Is(err, fs.ErrExist) {
						rollbackErrors = append(rollbackErrors, fmt.Errorf("remove new directory %q: %w", directory, err))
					}
				}
			}
			continue
		}
		if snapshot.identityBackup != "" {
			if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("remove replacement %q: %w", snapshot.path, err))
				continue
			}
			if err := os.Rename(snapshot.identityBackup, snapshot.path); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %q: %w", snapshot.path, err))
			}
			continue
		}
		if err := atomicWrite(snapshot.path, snapshot.data, snapshot.mode); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %q: %w", snapshot.path, err))
			continue
		}
		if err := os.Chtimes(snapshot.path, snapshot.modTime, snapshot.modTime); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore timestamps for %q: %w", snapshot.path, err))
		}
	}
	for _, snapshot := range t.snapshots {
		if snapshot.identityBackup != "" {
			_ = os.Remove(snapshot.identityBackup)
		}
	}
	return errors.Join(rollbackErrors...)
}

func (t *filesystemTransaction) commit() error {
	var cleanupErrors []error
	for _, snapshot := range t.snapshots {
		if snapshot.identityBackup == "" {
			continue
		}
		if err := os.Remove(snapshot.identityBackup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove rollback file %q: %w", snapshot.identityBackup, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func missingParentDirectories(directory string) []string {
	var missing []string
	for directory != "." {
		if _, err := os.Lstat(directory); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			break
		}
		missing = append(missing, directory)
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return missing
}

type state struct {
	Version         int                    `json:"version"`
	Executable      string                 `json:"executable"`
	ExecutableFile  fingerprint            `json:"executable_file"`
	Aliases         map[string]fingerprint `json:"aliases"`
	Shell           string                 `json:"shell,omitempty"`
	Profile         string                 `json:"profile,omitempty"`
	PathFile        string                 `json:"path_file,omitempty"`
	ProfileLine     string                 `json:"profile_line,omitempty"`
	PathEnabled     bool                   `json:"path_enabled,omitempty"`
	ProfileModified bool                   `json:"profile_modified,omitempty"`
	Enabled         *bool                  `json:"enabled,omitempty"`
}

type fingerprint struct {
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	ModTime int64  `json:"mod_time"`
	SHA256  string `json:"sha256"`
}

func DefaultRoot() string {
	return filepath.Join(filepath.Dir(config.GlobalConfigData()), "compatibility")
}

func New(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve compatibility directory: %w", err)
	}
	return &Manager{root: filepath.Clean(absolute)}, nil
}

func CanonicalAliases() []string {
	return slices.Clone(canonicalAliases)
}

func ModeForInvocation(executable, pathValue string) (InvocationMode, error) {
	resolved, err := resolveInvocation(executable, pathValue)
	if err != nil {
		return InvocationMode{}, nil
	}
	name := filepath.Base(resolved)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(strings.ToLower(name), ".exe")
	}
	if !slices.Contains(canonicalAliases, name) {
		return InvocationMode{}, nil
	}
	bin := filepath.Dir(resolved)
	if filepath.Base(bin) != "bin" {
		return InvocationMode{}, nil
	}
	manager, err := New(filepath.Dir(bin))
	if err != nil {
		return InvocationMode{}, err
	}
	current, err := manager.loadState()
	if errors.Is(err, fs.ErrNotExist) {
		return InvocationMode{}, nil
	}
	if err != nil {
		return InvocationMode{}, err
	}
	expected := filepath.Join(manager.binDirectory(), filepath.Base(resolved))
	if filepath.Clean(resolved) != expected {
		return InvocationMode{}, nil
	}
	if !current.managedAlias(name, expected) {
		return InvocationMode{}, fmt.Errorf("managed compatibility link %q is not recognized; run repair", expected)
	}
	return InvocationMode{Managed: true, Enabled: current.enabled(), Bin: manager.binDirectory(), Path: expected}, nil
}

func (m *Manager) Install(options Options) (Status, error) {
	target, targetInfo, targetFingerprint, err := resolveExecutable(options.Executable)
	if err != nil {
		return Status{}, err
	}
	if err := ensurePrivateDirectory(m.root); err != nil {
		return Status{}, err
	}
	bin := m.binDirectory()
	if err := ensurePrivateDirectory(bin); err != nil {
		return Status{}, err
	}

	previous, err := m.loadState()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Status{}, err
	}
	if err := m.validateLayout(); err != nil {
		return Status{}, err
	}
	if options.VerifyCompatibility {
		if err := m.verifyExecutable(target); err != nil {
			return Status{}, err
		}
	}
	if previous != nil {
		if previous.PathEnabled == options.SkipPath {
			return Status{}, errors.New("PATH setup choice differs from the installed state; run repair")
		}
		if options.Shell != "" && previous.Shell != options.Shell {
			return Status{}, errors.New("shell differs from the installed state; run repair")
		}
		if options.Profile != "" {
			profile, err := filepath.Abs(options.Profile)
			if err != nil {
				return Status{}, fmt.Errorf("resolve shell profile: %w", err)
			}
			if previous.Profile != filepath.Clean(profile) {
				return Status{}, errors.New("shell profile differs from the installed state; run repair")
			}
		}
		if previous.PathEnabled {
			if options.Shell == "" {
				options.Shell = previous.Shell
			}
			if options.Profile == "" {
				options.Profile = previous.Profile
			}
		}
	}

	created := make([]string, 0, len(canonicalAliases))
	aliases := make(map[string]fingerprint, len(canonicalAliases))
	for _, name := range canonicalAliases {
		path := filepath.Join(bin, name)
		aliasInfo, statErr := os.Lstat(path)
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			if err := os.Link(target, path); err != nil {
				removePaths(created)
				return Status{}, fmt.Errorf("create compatibility link %q: %w", path, err)
			}
			created = append(created, path)
		case statErr != nil:
			removePaths(created)
			return Status{}, fmt.Errorf("inspect compatibility link %q: %w", path, statErr)
		case aliasInfo.Mode()&os.ModeSymlink != 0 || !aliasInfo.Mode().IsRegular():
			removePaths(created)
			return Status{}, fmt.Errorf("compatibility link collision at %q", path)
		default:
			resolvedInfo, err := os.Stat(path)
			if err != nil {
				removePaths(created)
				return Status{}, fmt.Errorf("inspect compatibility link %q: %w", path, err)
			}
			if !os.SameFile(targetInfo, resolvedInfo) {
				removePaths(created)
				if previous != nil && previous.managedAlias(name, path) {
					return Status{}, fmt.Errorf("managed compatibility link %q is stale; run repair", path)
				}
				return Status{}, fmt.Errorf("compatibility link collision at %q", path)
			}
		}
		aliasFingerprint, err := fingerprintFile(path)
		if err != nil {
			removePaths(created)
			return Status{}, err
		}
		aliases[name] = aliasFingerprint
	}

	enabled := true
	if previous != nil {
		enabled = previous.enabled()
	}
	next := &state{
		Version:        stateVersion,
		Executable:     target,
		ExecutableFile: targetFingerprint,
		Aliases:        aliases,
		Enabled:        boolPointer(enabled),
	}
	next.PathEnabled = !options.SkipPath
	if next.PathEnabled {
		profileModified, err := m.setupPath(options, next, previous)
		if err != nil {
			removePaths(created)
			return Status{}, err
		}
		next.ProfileModified = profileModified
		if previous != nil && previous.Profile == next.Profile && previous.ProfileLine == next.ProfileLine {
			next.ProfileModified = previous.ProfileModified || profileModified
		}
	}
	if err := m.saveState(next); err != nil {
		removePaths(created)
		if previous == nil && next.ProfileModified {
			_ = removeProfileLine(next.Profile, next.ProfileLine)
		}
		if previous == nil && next.PathFile != "" {
			_ = os.Remove(next.PathFile)
		}
		return Status{}, err
	}
	return m.Status()
}

func (m *Manager) Status() (Status, error) {
	result := Status{Root: m.root, Bin: m.binDirectory()}
	current, err := m.loadState()
	if errors.Is(err, fs.ErrNotExist) {
		for _, name := range canonicalAliases {
			path := filepath.Join(result.Bin, name)
			aliasState := AliasMissing
			if _, statErr := os.Lstat(path); statErr == nil {
				aliasState = AliasCollision
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return Status{}, fmt.Errorf("inspect compatibility link %q: %w", path, statErr)
			}
			result.Aliases = append(result.Aliases, AliasStatus{Name: name, Path: path, State: aliasState})
		}
		result.PathActive = pathContains(result.Bin, os.Getenv("PATH"))
		return result, nil
	}
	if err != nil {
		return Status{}, err
	}
	result.Executable = current.Executable
	result.Profile = current.Profile
	result.Enabled = current.enabled()
	result.PathSetup = current.pathConfigured()
	result.PathActive = pathContains(result.Bin, os.Getenv("PATH"))

	targetInfo, targetErr := os.Stat(current.Executable)
	for _, name := range canonicalAliases {
		path := filepath.Join(result.Bin, name)
		aliasState, err := current.aliasState(name, path, targetInfo, targetErr)
		if err != nil {
			return Status{}, err
		}
		result.Aliases = append(result.Aliases, AliasStatus{Name: name, Path: path, State: aliasState})
	}
	result.Installed = targetErr == nil && (!current.PathEnabled || result.PathSetup)
	for _, alias := range result.Aliases {
		if alias.State != AliasLinked {
			result.Installed = false
		}
	}
	return result, nil
}

func (m *Manager) SetEnabled(enabled bool) (Status, error) {
	current, err := m.loadState()
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, errors.New("compatibility aliases are not installed")
	}
	if err != nil {
		return Status{}, err
	}
	if err := m.validateLayout(); err != nil {
		return Status{}, err
	}
	status, err := m.Status()
	if err != nil {
		return Status{}, err
	}
	if !status.Installed {
		return Status{}, errors.New("compatibility aliases need repair before changing mode")
	}
	current.Enabled = boolPointer(enabled)
	if err := m.saveState(current); err != nil {
		return Status{}, err
	}
	return m.Status()
}

func (m *Manager) Enable() (Status, error) {
	return m.SetEnabled(true)
}

func (m *Manager) Disable() (Status, error) {
	return m.SetEnabled(false)
}

func (m *Manager) Repair(options Options) (Status, error) {
	current, err := m.loadState()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m.Install(options)
		}
		return Status{}, err
	}
	if strings.TrimSpace(options.Executable) == "" {
		options.Executable = current.Executable
	}
	target, targetInfo, targetFingerprint, err := resolveExecutable(options.Executable)
	if err != nil {
		return Status{}, err
	}
	if err := ensurePrivateDirectory(m.root); err != nil {
		return Status{}, err
	}
	if err := ensurePrivateDirectory(m.binDirectory()); err != nil {
		return Status{}, err
	}
	if err := m.validateLayout(); err != nil {
		return Status{}, err
	}
	if options.VerifyCompatibility {
		if err := m.verifyExecutable(target); err != nil {
			return Status{}, err
		}
	}
	if err := m.preflightManagedPath(current); err != nil {
		return Status{}, err
	}

	aliasStates := make(map[string]AliasState, len(canonicalAliases))
	for _, name := range canonicalAliases {
		path := filepath.Join(m.binDirectory(), name)
		aliasState, stateErr := current.aliasState(name, path, targetInfo, nil)
		if stateErr != nil {
			return Status{}, stateErr
		}
		if aliasState == AliasCollision {
			return Status{}, fmt.Errorf("compatibility link collision at %q", path)
		}
		aliasStates[name] = aliasState
	}

	next := &state{
		Version:         stateVersion,
		Executable:      target,
		ExecutableFile:  targetFingerprint,
		Aliases:         make(map[string]fingerprint, len(canonicalAliases)),
		Shell:           current.Shell,
		Profile:         current.Profile,
		PathFile:        current.PathFile,
		ProfileLine:     current.ProfileLine,
		PathEnabled:     current.PathEnabled,
		ProfileModified: current.ProfileModified,
		Enabled:         boolPointer(current.enabled()),
	}

	var desiredPath *pathSetupPlan
	if !options.SkipPath {
		if options.Shell == "" {
			options.Shell = current.Shell
		}
		if options.Profile == "" {
			options.Profile = current.Profile
		}
		desiredPath, err = m.planPathSetup(options, current)
		if err != nil {
			return Status{}, err
		}
	}

	transaction := &filesystemTransaction{watched: make(map[string]struct{})}
	for _, name := range canonicalAliases {
		if err := transaction.watch(filepath.Join(m.binDirectory(), name), true); err != nil {
			return Status{}, transaction.fail(err)
		}
	}
	for _, path := range []string{m.statePath(), current.Profile, current.PathFile} {
		if err := transaction.watch(path, false); err != nil {
			return Status{}, transaction.fail(err)
		}
	}
	if desiredPath != nil {
		for _, path := range []string{desiredPath.profile, desiredPath.pathFile} {
			if err := transaction.watch(path, false); err != nil {
				return Status{}, transaction.fail(err)
			}
		}
	}

	for _, name := range canonicalAliases {
		path := filepath.Join(m.binDirectory(), name)
		switch aliasStates[name] {
		case AliasMissing:
			if err := os.Link(target, path); err != nil {
				return Status{}, transaction.fail(fmt.Errorf("create compatibility link %q: %w", path, err))
			}
		case AliasStale:
			if err := os.Remove(path); err != nil {
				return Status{}, transaction.fail(fmt.Errorf("remove stale compatibility link %q: %w", path, err))
			}
			if err := os.Link(target, path); err != nil {
				return Status{}, transaction.fail(fmt.Errorf("replace compatibility link %q: %w", path, err))
			}
		}
		aliasFingerprint, err := fingerprintFile(path)
		if err != nil {
			return Status{}, transaction.fail(err)
		}
		next.Aliases[name] = aliasFingerprint
	}

	if options.SkipPath {
		if current.PathEnabled && current.ProfileModified {
			if err := m.removeProfileLine(current.Profile, current.ProfileLine); err != nil {
				return Status{}, transaction.fail(err)
			}
		}
		if current.PathEnabled && current.PathFile != "" {
			if err := os.Remove(current.PathFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return Status{}, transaction.fail(fmt.Errorf("remove PATH setup %q: %w", current.PathFile, err))
			}
		}
		next.PathEnabled = false
		next.Shell = ""
		next.Profile = ""
		next.PathFile = ""
		next.ProfileLine = ""
		next.ProfileModified = false
	} else {
		profileChanged := current.PathEnabled && (desiredPath.shell != current.Shell || desiredPath.profile != current.Profile)
		modified, err := m.applyPathSetup(desiredPath)
		if err != nil {
			return Status{}, transaction.fail(err)
		}
		if profileChanged {
			if current.ProfileModified && (current.Profile != desiredPath.profile || current.ProfileLine != desiredPath.profileLine) {
				if err := m.removeProfileLine(current.Profile, current.ProfileLine); err != nil {
					return Status{}, transaction.fail(err)
				}
			}
			if current.PathFile != "" && current.PathFile != desiredPath.pathFile {
				if err := os.Remove(current.PathFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return Status{}, transaction.fail(fmt.Errorf("remove PATH setup %q: %w", current.PathFile, err))
				}
			}
			if current.Profile == desiredPath.profile && current.ProfileLine == desiredPath.profileLine {
				next.ProfileModified = current.ProfileModified || modified
			} else {
				next.ProfileModified = modified
			}
		} else {
			next.ProfileModified = current.ProfileModified || modified
		}
		next.PathEnabled = true
		next.Shell = desiredPath.shell
		next.Profile = desiredPath.profile
		next.PathFile = desiredPath.pathFile
		next.ProfileLine = desiredPath.profileLine
	}
	if err := m.saveState(next); err != nil {
		return Status{}, transaction.fail(err)
	}
	if err := transaction.commit(); err != nil {
		return Status{}, err
	}
	return m.Status()
}

func (m *Manager) Uninstall() (Status, error) {
	current, err := m.loadState()
	if errors.Is(err, fs.ErrNotExist) {
		return m.Status()
	}
	if err != nil {
		return Status{}, err
	}
	if err := m.validateLayout(); err != nil {
		return Status{}, err
	}
	if err := m.preflightManagedPath(current); err != nil {
		return Status{}, err
	}

	aliases := make([]string, 0, len(canonicalAliases))
	for _, name := range canonicalAliases {
		path := filepath.Join(m.binDirectory(), name)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return Status{}, fmt.Errorf("inspect compatibility link %q: %w", path, err)
		}
		if !current.managedAlias(name, path) {
			return Status{}, fmt.Errorf("refusing to remove unrecognized file %q", path)
		}
		aliases = append(aliases, path)
	}

	transaction := &filesystemTransaction{watched: make(map[string]struct{})}
	for _, path := range aliases {
		if err := transaction.watch(path, true); err != nil {
			return Status{}, transaction.fail(err)
		}
	}
	for _, path := range []string{current.Profile, current.PathFile, m.statePath()} {
		if err := transaction.watch(path, false); err != nil {
			return Status{}, transaction.fail(err)
		}
	}

	for _, path := range aliases {
		if err := os.Remove(path); err != nil {
			return Status{}, transaction.fail(fmt.Errorf("remove compatibility link %q: %w", path, err))
		}
	}
	if current.ProfileModified && current.Profile != "" && current.ProfileLine != "" {
		if err := m.removeProfileLine(current.Profile, current.ProfileLine); err != nil {
			return Status{}, transaction.fail(err)
		}
	}
	if current.PathFile != "" {
		if err := os.Remove(current.PathFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Status{}, transaction.fail(fmt.Errorf("remove PATH setup %q: %w", current.PathFile, err))
		}
	}
	if err := os.Remove(m.statePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Status{}, transaction.fail(fmt.Errorf("remove compatibility state: %w", err))
	}
	if err := transaction.commit(); err != nil {
		return Status{}, err
	}
	_ = os.Remove(m.binDirectory())
	_ = os.Remove(m.root)
	return m.Status()
}

func (m *Manager) setupPath(options Options, next *state, current *state) (bool, error) {
	if err := m.preflightManagedPath(current); err != nil {
		return false, err
	}
	plan, err := m.planPathSetup(options, current)
	if err != nil {
		return false, err
	}
	transaction := &filesystemTransaction{watched: make(map[string]struct{})}
	for _, path := range []string{plan.profile, plan.pathFile} {
		if err := transaction.watch(path, false); err != nil {
			return false, transaction.fail(err)
		}
	}
	modified, err := m.applyPathSetup(plan)
	if err != nil {
		return false, transaction.fail(err)
	}
	next.Shell = plan.shell
	next.Profile = plan.profile
	next.PathFile = plan.pathFile
	next.ProfileLine = plan.profileLine
	if err := transaction.commit(); err != nil {
		return false, err
	}
	return modified, nil
}

func (m *Manager) preflightManagedPath(current *state) error {
	if current == nil || !current.PathEnabled {
		return nil
	}
	if current.PathFile != "" {
		info, err := os.Lstat(current.PathFile)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("managed compatibility PATH setup %q is not a regular file", current.PathFile)
			}
			data, readErr := os.ReadFile(current.PathFile)
			if readErr != nil {
				return fmt.Errorf("read managed compatibility PATH setup %q: %w", current.PathFile, readErr)
			}
			if string(data) != pathScript(current.Shell, m.binDirectory()) {
				return fmt.Errorf("managed compatibility PATH setup %q has changed; refusing to replace it", current.PathFile)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect managed compatibility PATH setup %q: %w", current.PathFile, err)
		}
	}
	if current.ProfileModified && current.Profile != "" {
		if info, err := os.Lstat(current.Profile); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("shell profile %q is not a regular file", current.Profile)
			}
			if _, err := os.ReadFile(current.Profile); err != nil {
				return fmt.Errorf("read shell profile %q: %w", current.Profile, err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect shell profile %q: %w", current.Profile, err)
		}
	}
	return nil
}

func (m *Manager) planPathSetup(options Options, current *state) (*pathSetupPlan, error) {
	shell, profile, err := resolveShellProfile(options.Shell, options.Profile)
	if err != nil {
		return nil, err
	}
	extension := ".sh"
	if shell == "fish" {
		extension = ".fish"
	}
	plan := &pathSetupPlan{
		shell:       shell,
		profile:     profile,
		pathFile:    filepath.Join(m.root, "path"+extension),
		pathContent: pathScript(shell, m.binDirectory()),
	}
	plan.profileLine = "source " + shellQuote(plan.pathFile)

	profileData, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read shell profile %q: %w", profile, err)
	}
	if info, statErr := os.Lstat(profile); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("shell profile %q is not a regular file", profile)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect shell profile %q: %w", profile, statErr)
	}
	managedLine := current != nil && current.PathEnabled && current.Profile == profile && current.ProfileLine == plan.profileLine
	if containsLine(string(profileData), plan.profileLine) && !managedLine {
		return nil, fmt.Errorf("shell profile %q already contains an unmanaged compatibility PATH line", profile)
	}

	if info, statErr := os.Lstat(plan.pathFile); statErr == nil {
		managedFile := current != nil && current.PathEnabled && current.PathFile == plan.pathFile
		if !managedFile {
			return nil, fmt.Errorf("unmanaged compatibility PATH setup already exists at %q", plan.pathFile)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("managed compatibility PATH setup %q is not a regular file", plan.pathFile)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect compatibility PATH setup %q: %w", plan.pathFile, statErr)
	}
	return plan, nil
}

func (m *Manager) applyPathSetup(plan *pathSetupPlan) (bool, error) {
	if err := m.inject(operationWritePath); err != nil {
		return false, fmt.Errorf("write compatibility PATH setup: %w", err)
	}
	if err := atomicWrite(plan.pathFile, []byte(plan.pathContent), 0o600); err != nil {
		return false, fmt.Errorf("write compatibility PATH setup: %w", err)
	}
	if err := m.inject(operationUpdateProfile); err != nil {
		return false, fmt.Errorf("update shell profile %q: %w", plan.profile, err)
	}
	modified, err := ensureProfileLine(plan.profile, plan.profileLine)
	if err != nil {
		return false, err
	}
	return modified, nil
}

func (m *Manager) removeProfileLine(profile, line string) error {
	if err := m.inject(operationUpdateProfile); err != nil {
		return fmt.Errorf("update shell profile %q: %w", profile, err)
	}
	return removeProfileLine(profile, line)
}

func (m *Manager) inject(operation string) error {
	if m.failForTest == nil {
		return nil
	}
	return m.failForTest(operation)
}

func (m *Manager) loadState() (*state, error) {
	data, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil, err
	}
	var current state
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, fmt.Errorf("decode compatibility state: %w", err)
	}
	if current.Version != stateVersion {
		return nil, fmt.Errorf("unsupported compatibility state version %d", current.Version)
	}
	return &current, nil
}

func (m *Manager) saveState(current *state) error {
	if err := m.inject(operationSaveState); err != nil {
		return fmt.Errorf("write compatibility state: %w", err)
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("encode compatibility state: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(m.statePath(), data, 0o600); err != nil {
		return fmt.Errorf("write compatibility state: %w", err)
	}
	return nil
}

func (m *Manager) binDirectory() string {
	return filepath.Join(m.root, "bin")
}

func (m *Manager) statePath() string {
	return filepath.Join(m.root, "state.json")
}

func (m *Manager) validateLayout() error {
	if err := validateDirectoryEntries(m.root, []string{"bin", "state.json", "path.sh", "path.fish"}); err != nil {
		return err
	}
	return validateDirectoryEntries(m.binDirectory(), canonicalAliases)
}

func (m *Manager) verifyExecutable(executable string) error {
	probeDirectory, err := os.MkdirTemp(m.root, ".compatibility-probe-")
	if err != nil {
		return fmt.Errorf("create compatibility probe directory: %w", err)
	}
	defer os.RemoveAll(probeDirectory)
	probe := filepath.Join(probeDirectory, "codex")
	if err := os.Link(executable, probe); err != nil {
		return fmt.Errorf("create compatibility probe link: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, probe, "--version")
	command.Env = removeEnvironment(os.Environ(), "CRUX_COMPATIBILITY_BYPASS")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("selected executable does not provide the compatibility layer: %w", err)
	}
	if string(output) != "codex-cli 0.149.1\n" {
		return fmt.Errorf("selected executable does not provide the compatibility layer")
	}
	return nil
}

func (s *state) aliasState(name, path string, targetInfo os.FileInfo, targetErr error) (AliasState, error) {
	aliasInfo, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return AliasMissing, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect compatibility link %q: %w", path, err)
	}
	if aliasInfo.Mode()&os.ModeSymlink != 0 || !aliasInfo.Mode().IsRegular() {
		return AliasCollision, nil
	}
	if targetErr == nil {
		resolvedInfo, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("inspect compatibility link %q: %w", path, err)
		}
		if os.SameFile(targetInfo, resolvedInfo) {
			return AliasLinked, nil
		}
	}
	if s.managedAlias(name, path) {
		return AliasStale, nil
	}
	return AliasCollision, nil
}

func (s *state) managedAlias(name, path string) bool {
	expected, ok := s.Aliases[name]
	if !ok {
		return false
	}
	actual, err := fingerprintFile(path)
	return err == nil && actual == expected
}

func (s *state) enabled() bool {
	return s.Enabled == nil || *s.Enabled
}

func boolPointer(value bool) *bool {
	return &value
}

func (s *state) pathConfigured() bool {
	if !s.PathEnabled || s.Profile == "" || s.ProfileLine == "" || s.PathFile == "" {
		return false
	}
	profileData, err := os.ReadFile(s.Profile)
	if err != nil || !containsLine(string(profileData), s.ProfileLine) {
		return false
	}
	_, err = os.Stat(s.PathFile)
	return err == nil
}

func resolveInvocation(executable, pathValue string) (string, error) {
	if strings.ContainsRune(executable, os.PathSeparator) || (runtime.GOOS == "windows" && strings.Contains(executable, "/")) {
		absolute, err := filepath.Abs(executable)
		if err != nil {
			return "", err
		}
		return filepath.Clean(absolute), nil
	}
	candidates := []string{executable}
	if runtime.GOOS == "windows" && filepath.Ext(executable) == "" {
		candidates = append(candidates, executable+".exe")
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		for _, name := range candidates {
			candidate, err := filepath.Abs(filepath.Join(directory, name))
			if err != nil {
				continue
			}
			info, err := os.Stat(candidate)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				continue
			}
			return filepath.Clean(candidate), nil
		}
	}
	return "", fs.ErrNotExist
}

func resolveExecutable(path string) (string, os.FileInfo, fingerprint, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil, fingerprint{}, errors.New("Crux executable path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fingerprint{}, fmt.Errorf("resolve Crux executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, fingerprint{}, fmt.Errorf("resolve Crux executable %q: %w", absolute, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fingerprint{}, fmt.Errorf("inspect Crux executable %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fingerprint{}, fmt.Errorf("Crux executable %q is not a regular file", resolved)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", nil, fingerprint{}, fmt.Errorf("Crux executable %q is not executable", resolved)
	}
	value, err := fingerprintFile(resolved)
	if err != nil {
		return "", nil, fingerprint{}, err
	}
	return filepath.Clean(resolved), info, value, nil
}

func fingerprintFile(path string) (fingerprint, error) {
	file, err := os.Open(path)
	if err != nil {
		return fingerprint{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := file.WriteTo(hash); err != nil {
		return fingerprint{}, fmt.Errorf("hash %q: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		return fingerprint{}, fmt.Errorf("inspect %q: %w", path, err)
	}
	return fingerprint{
		Size:    info.Size(),
		Mode:    uint32(info.Mode()),
		ModTime: info.ModTime().UnixNano(),
		SHA256:  hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func ensurePrivateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("private compatibility path %q is not a directory", path)
		}
		return validatePrivateAccess(path, info)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect private compatibility path %q: %w", path, err)
	}
	if err := createPrivateDirectory(path); err != nil {
		return fmt.Errorf("create private compatibility directory %q: %w", path, err)
	}
	return nil
}

func validateDirectoryEntries(path string, allowed []string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect private compatibility directory %q: %w", path, err)
	}
	for _, entry := range entries {
		if !slices.Contains(allowed, entry.Name()) {
			return fmt.Errorf("private compatibility directory %q contains unrecognized entry %q", path, entry.Name())
		}
	}
	return nil
}

func resolveShellProfile(shell, profile string) (string, string, error) {
	if shell == "" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	shell = strings.TrimPrefix(strings.TrimSpace(shell), "-")
	if shell == "" {
		return "", "", errors.New("shell is required for PATH setup")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home directory: %w", err)
	}
	if profile == "" {
		switch shell {
		case "zsh":
			profile = filepath.Join(home, ".zshrc")
		case "bash":
			profile = filepath.Join(home, ".bashrc")
		case "sh":
			profile = filepath.Join(home, ".profile")
		case "fish":
			profile = filepath.Join(home, ".config", "fish", "config.fish")
		default:
			return "", "", fmt.Errorf("unsupported shell %q; specify zsh, bash, sh, or fish", shell)
		}
	}
	absolute, err := filepath.Abs(profile)
	if err != nil {
		return "", "", fmt.Errorf("resolve shell profile: %w", err)
	}
	return shell, filepath.Clean(absolute), nil
}

func pathScript(shell, bin string) string {
	quoted := shellQuote(bin)
	if shell == "fish" {
		return "fish_add_path --prepend --move " + quoted + "\n"
	}
	return "export PATH=" + quoted + ":\"$PATH\"\n"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func ensureProfileLine(profile, line string) (bool, error) {
	if info, err := os.Lstat(profile); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("shell profile %q is not a regular file", profile)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspect shell profile %q: %w", profile, err)
	}
	data, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read shell profile %q: %w", profile, err)
	}
	if containsLine(string(data), line) {
		return false, nil
	}
	content := string(data)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(profile); err == nil {
		mode = info.Mode().Perm()
	}
	if err := atomicWrite(profile, []byte(content), mode); err != nil {
		return false, fmt.Errorf("update shell profile %q: %w", profile, err)
	}
	return true, nil
}

func removeProfileLine(profile, line string) error {
	data, err := os.ReadFile(profile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read shell profile %q: %w", profile, err)
	}
	parts := strings.SplitAfter(string(data), "\n")
	removed := false
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		candidate := strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r")
		if !removed && candidate == line {
			removed = true
			continue
		}
		kept = append(kept, part)
	}
	if !removed {
		return nil
	}
	info, err := os.Stat(profile)
	if err != nil {
		return fmt.Errorf("inspect shell profile %q: %w", profile, err)
	}
	if err := atomicWrite(profile, []byte(strings.Join(kept, "")), info.Mode().Perm()); err != nil {
		return fmt.Errorf("update shell profile %q: %w", profile, err)
	}
	return nil
}

func containsLine(content, line string) bool {
	for candidate := range strings.Lines(content) {
		if strings.TrimSuffix(strings.TrimSuffix(candidate, "\n"), "\r") == line {
			return true
		}
	}
	return false
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".crux-compatibility-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func pathContains(directory, pathValue string) bool {
	for _, entry := range filepath.SplitList(pathValue) {
		if filepath.Clean(entry) == filepath.Clean(directory) {
			return true
		}
	}
	return false
}

func removeEnvironment(environment []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func removePaths(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}
