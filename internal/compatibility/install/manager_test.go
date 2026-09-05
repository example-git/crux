package install

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallIsIdempotentAndUninstallIsReversible(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	profile := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(profile, []byte("export EXAMPLE=value\n"), 0o640))
	executable := testExecutable(t, "crux", "version-one")
	manager, err := New(root)
	require.NoError(t, err)
	options := Options{Executable: executable, Shell: "zsh", Profile: profile}

	status, err := manager.Install(options)
	require.NoError(t, err)
	require.True(t, status.Installed)
	require.True(t, status.PathSetup)
	require.Len(t, status.Aliases, len(canonicalAliases))
	for _, alias := range status.Aliases {
		require.Equal(t, AliasLinked, alias.State)
		targetInfo, statErr := os.Stat(executable)
		require.NoError(t, statErr)
		aliasInfo, statErr := os.Stat(alias.Path)
		require.NoError(t, statErr)
		require.True(t, os.SameFile(targetInfo, aliasInfo))
	}
	requireMode(t, root, 0o700)
	requireMode(t, filepath.Join(root, "bin"), 0o700)
	requireMode(t, filepath.Join(root, "state.json"), 0o600)
	requireMode(t, filepath.Join(root, "path.sh"), 0o600)
	pathSetup, err := os.ReadFile(filepath.Join(root, "path.sh"))
	require.NoError(t, err)
	require.Equal(t, "export PATH="+shellQuote(filepath.Join(root, "bin"))+":\"$PATH\"\n", string(pathSetup))

	firstProfile, err := os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(firstProfile), "source "))
	_, err = manager.Install(options)
	require.NoError(t, err)
	secondProfile, err := os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, firstProfile, secondProfile)

	status, err = manager.Uninstall()
	require.NoError(t, err)
	require.False(t, status.Installed)
	for _, alias := range status.Aliases {
		require.Equal(t, AliasMissing, alias.State)
	}
	profileData, err := os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, "export EXAMPLE=value\n", string(profileData))
	_, err = os.Stat(filepath.Join(root, "state.json"))
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = manager.Uninstall()
	require.NoError(t, err)
}

func TestCompatibilityModeToggleIsPersistentAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	status, err := manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	require.True(t, status.Enabled)

	status, err = manager.Disable()
	require.NoError(t, err)
	require.False(t, status.Enabled)
	status, err = manager.Disable()
	require.NoError(t, err)
	require.False(t, status.Enabled)

	status, err = manager.Enable()
	require.NoError(t, err)
	require.True(t, status.Enabled)
	status, err = manager.Enable()
	require.NoError(t, err)
	require.True(t, status.Enabled)
}

func TestCompatibilityModeDefaultsLegacyStateToEnabled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	current, err := manager.loadState()
	require.NoError(t, err)
	current.Enabled = nil
	require.NoError(t, manager.saveState(current))

	status, err := manager.Status()
	require.NoError(t, err)
	require.True(t, status.Enabled)
}

func TestCompatibilityModeRequiresHealthyInstallation(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "compatibility"))
	require.NoError(t, err)
	_, err = manager.Disable()
	require.ErrorContains(t, err, "not installed")

	executable := testExecutable(t, "crux", "crux")
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(manager.binDirectory(), "codex")))
	_, err = manager.Disable()
	require.ErrorContains(t, err, "need repair")
}

func TestModeForInvocationRecognizesOnlyManagedAliases(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	_, err = manager.Disable()
	require.NoError(t, err)
	bin := manager.binDirectory()

	mode, err := ModeForInvocation(filepath.Join(bin, "codex"), bin)
	require.NoError(t, err)
	require.True(t, mode.Managed)
	require.False(t, mode.Enabled)
	require.Equal(t, bin, mode.Bin)

	mode, err = ModeForInvocation("codex", bin)
	require.NoError(t, err)
	require.True(t, mode.Managed)

	mode, err = ModeForInvocation(testExecutable(t, "codex", "official"), bin)
	require.NoError(t, err)
	require.False(t, mode.Managed)
}

func TestInstallVerifiesCompatibilityExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	compatible := testExecutable(t, "crux", "#!/bin/sh\nprintf 'codex-cli 0.149.1\\n'\n")
	manager, err := New(filepath.Join(t.TempDir(), "compatibility"))
	require.NoError(t, err)
	status, err := manager.Install(Options{Executable: compatible, SkipPath: true, VerifyCompatibility: true})
	require.NoError(t, err)
	require.True(t, status.Installed)

	incompatible := testExecutable(t, "crux", "#!/bin/sh\nprintf 'crux development\\n'\n")
	root := filepath.Join(t.TempDir(), "compatibility")
	manager, err = New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: incompatible, SkipPath: true, VerifyCompatibility: true})
	require.ErrorContains(t, err, "does not provide the compatibility layer")
	for _, name := range canonicalAliases {
		_, statErr := os.Stat(filepath.Join(root, "bin", name))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
}

func TestInstallRejectsAliasCollisionWithoutReplacingIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	collision := filepath.Join(bin, "codex")
	require.NoError(t, os.WriteFile(collision, []byte("official tool"), 0o700))
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)

	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.ErrorContains(t, err, "collision")
	data, readErr := os.ReadFile(collision)
	require.NoError(t, readErr)
	require.Equal(t, "official tool", string(data))
	for _, name := range canonicalAliases[1:] {
		_, statErr := os.Stat(filepath.Join(bin, name))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
}

func TestRepairReplacesOnlyManagedStaleLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "version-one")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)

	replacement := testExecutable(t, "replacement", "version-two")
	require.NoError(t, os.Rename(replacement, executable))
	status, err := manager.Status()
	require.NoError(t, err)
	for _, alias := range status.Aliases {
		require.Equal(t, AliasStale, alias.State)
	}

	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.ErrorContains(t, err, "run repair")
	status, err = manager.Repair(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	require.True(t, status.Installed)
	targetInfo, err := os.Stat(executable)
	require.NoError(t, err)
	for _, alias := range status.Aliases {
		require.Equal(t, AliasLinked, alias.State)
		aliasInfo, statErr := os.Stat(alias.Path)
		require.NoError(t, statErr)
		require.True(t, os.SameFile(targetInfo, aliasInfo))
	}
}

func TestRepairRestoresMissingLinkAndRefusesCollision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)

	claude := filepath.Join(root, "bin", "claude")
	require.NoError(t, os.Remove(claude))
	_, err = manager.Repair(Options{SkipPath: true})
	require.NoError(t, err)
	targetInfo, err := os.Stat(executable)
	require.NoError(t, err)
	aliasInfo, err := os.Stat(claude)
	require.NoError(t, err)
	require.True(t, os.SameFile(targetInfo, aliasInfo))

	codex := filepath.Join(root, "bin", "codex")
	require.NoError(t, os.Remove(codex))
	require.NoError(t, os.WriteFile(codex, []byte("collision"), 0o700))
	_, err = manager.Repair(Options{SkipPath: true})
	require.ErrorContains(t, err, "collision")
	data, readErr := os.ReadFile(codex)
	require.NoError(t, readErr)
	require.Equal(t, "collision", string(data))
}

func TestUninstallPreflightsLastAliasCollisionBeforeRemovingAnything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)

	copilot := filepath.Join(root, "bin", "copilot")
	require.NoError(t, os.Remove(copilot))
	require.NoError(t, os.WriteFile(copilot, []byte("replacement"), 0o700))
	stateBefore, err := os.ReadFile(manager.statePath())
	require.NoError(t, err)
	_, err = manager.Uninstall()
	require.ErrorContains(t, err, "refusing to remove unrecognized file")
	data, readErr := os.ReadFile(copilot)
	require.NoError(t, readErr)
	require.Equal(t, "replacement", string(data))
	for _, name := range canonicalAliases[:len(canonicalAliases)-1] {
		aliasInfo, statErr := os.Stat(filepath.Join(manager.binDirectory(), name))
		require.NoError(t, statErr)
		targetInfo, statErr := os.Stat(executable)
		require.NoError(t, statErr)
		require.True(t, os.SameFile(targetInfo, aliasInfo))
	}
	stateAfter, err := os.ReadFile(manager.statePath())
	require.NoError(t, err)
	require.Equal(t, stateBefore, stateAfter)
}

func TestRepairRollsBackPathProfileAndStateFailures(t *testing.T) {
	for _, operation := range []string{operationWritePath, operationUpdateProfile, operationSaveState} {
		t.Run(operation, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "compatibility")
			oldProfile := filepath.Join(t.TempDir(), ".zshrc")
			newProfile := filepath.Join(t.TempDir(), "config.fish")
			require.NoError(t, os.WriteFile(oldProfile, []byte("existing\n"), 0o640))
			executable := testExecutable(t, "crux", "crux")
			manager, err := New(root)
			require.NoError(t, err)
			_, err = manager.Install(Options{Executable: executable, Shell: "zsh", Profile: oldProfile})
			require.NoError(t, err)

			stateBefore, err := os.ReadFile(manager.statePath())
			require.NoError(t, err)
			profileBefore, err := os.ReadFile(oldProfile)
			require.NoError(t, err)
			pathBefore, err := os.ReadFile(filepath.Join(root, "path.sh"))
			require.NoError(t, err)
			manager.failForTest = func(candidate string) error {
				if candidate == operation {
					return errors.New("injected failure")
				}
				return nil
			}

			_, err = manager.Repair(Options{Shell: "fish", Profile: newProfile})
			require.ErrorContains(t, err, "injected failure")
			manager.failForTest = nil

			stateAfter, err := os.ReadFile(manager.statePath())
			require.NoError(t, err)
			require.Equal(t, stateBefore, stateAfter)
			profileAfter, err := os.ReadFile(oldProfile)
			require.NoError(t, err)
			require.Equal(t, profileBefore, profileAfter)
			pathAfter, err := os.ReadFile(filepath.Join(root, "path.sh"))
			require.NoError(t, err)
			require.Equal(t, pathBefore, pathAfter)
			_, err = os.Stat(newProfile)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(filepath.Join(root, "path.fish"))
			require.ErrorIs(t, err, os.ErrNotExist)

			status, err := manager.Status()
			require.NoError(t, err)
			require.True(t, status.Installed)
			require.True(t, status.PathSetup)
			for _, alias := range status.Aliases {
				require.Equal(t, AliasLinked, alias.State)
			}
		})
	}
}

func TestInstallRejectsUnmanagedProfileLine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	profile := filepath.Join(t.TempDir(), ".zshrc")
	line := "source " + shellQuote(filepath.Join(root, "path.sh")) + "\n"
	require.NoError(t, os.WriteFile(profile, []byte(line), 0o600))
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)

	_, err = manager.Install(Options{Executable: executable, Shell: "zsh", Profile: profile})
	require.ErrorContains(t, err, "unmanaged compatibility PATH line")
	data, readErr := os.ReadFile(profile)
	require.NoError(t, readErr)
	require.Equal(t, line, string(data))
}

func TestRepairRestoresAndDisablesPathSetup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compatibility")
	profile := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(profile, []byte("existing\n"), 0o600))
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, Shell: "zsh", Profile: profile})
	require.NoError(t, err)

	current, err := manager.loadState()
	require.NoError(t, err)
	require.NoError(t, removeProfileLine(profile, current.ProfileLine))
	require.NoError(t, os.Remove(current.PathFile))
	status, err := manager.Repair(Options{})
	require.NoError(t, err)
	require.True(t, status.PathSetup)
	profileData, err := os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(profileData), current.ProfileLine))

	status, err = manager.Repair(Options{SkipPath: true})
	require.NoError(t, err)
	require.True(t, status.Installed)
	require.False(t, status.PathSetup)
	profileData, err = os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, "existing\n", string(profileData))
	_, err = os.Stat(current.PathFile)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestInstallRejectsInsecureOrUnrecognizedPrivateDirectory(t *testing.T) {
	executable := testExecutable(t, "crux", "crux")

	insecure := filepath.Join(t.TempDir(), "compatibility")
	require.NoError(t, os.Mkdir(insecure, 0o755))
	require.NoError(t, os.Chmod(insecure, 0o755))
	manager, err := New(insecure)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.ErrorContains(t, err, "expected no group or other access")

	unrecognized := filepath.Join(t.TempDir(), "compatibility")
	require.NoError(t, os.Mkdir(unrecognized, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(unrecognized, "unrelated"), []byte("keep"), 0o600))
	manager, err = New(unrecognized)
	require.NoError(t, err)
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.ErrorContains(t, err, "contains unrecognized entry")
	data, readErr := os.ReadFile(filepath.Join(unrecognized, "unrelated"))
	require.NoError(t, readErr)
	require.Equal(t, "keep", string(data))
}

func TestInstallRejectsSymlinkedPrivateDirectory(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	root := filepath.Join(parent, "compatibility")
	require.NoError(t, os.Symlink(target, root))
	manager, err := New(root)
	require.NoError(t, err)
	executable := testExecutable(t, "crux", "crux")

	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.ErrorContains(t, err, "is not a directory")
}

func testExecutable(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o700))
	return path
}

func requireMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, expected, info.Mode().Perm())
}
