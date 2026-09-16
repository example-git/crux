package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func TestValidateCloneURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "empty", url: "", wantErr: "required"},
		{name: "leading dash", url: "-oProxyCommand=evil", wantErr: "invalid"},
		{name: "https", url: "https://example.com/repo.git"},
		{name: "http", url: "http://example.com/repo.git"},
		{name: "ssh scheme", url: "ssh://git@example.com/repo.git"},
		{name: "scp-like", url: "git@example.com:org/repo.git"},
		{name: "bare local path", url: "/tmp/repo", wantErr: "unsupported"},
		{name: "file scheme", url: "file:///tmp/repo", wantErr: "unsupported"},
		{name: "https missing host", url: "https:///repo.git", wantErr: "invalid"},
		{name: "malformed url", url: "https://[::1", wantErr: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCloneURL(test.url)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestResolveProjectTargetRejectsTraversal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	server := NewServer(nil, "unix", "test")
	server.workspaceRoots = []string{root}

	target, err := server.resolveProjectTarget(root, "my-project")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "my-project"), target)

	target, err = server.resolveProjectTarget(root, "nested/dir")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "nested", "dir"), target)

	// A leading ".." is neutralized by resolveProjectTarget's synthetic-root
	// cleaning rather than producing an escaping path: the result must stay
	// confined under root, never outside it.
	target, err = server.resolveProjectTarget(root, "../escape")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "escape"), target)
	require.True(t, fsext.HasPrefix(target, root))

	_, err = server.resolveProjectTarget(root, "..")
	require.Error(t, err)

	_, err = server.resolveProjectTarget(t.TempDir(), "project")
	require.ErrorContains(t, err, "not a configured workspace root")

	_, err = server.resolveProjectTarget(root, "")
	require.Error(t, err)
}

func TestCreateProjectPlainMode(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))

	path, err := server.createProject(t.Context(), proto.PeerWorkspaceCreateRequest{
		Root: root, RelativePath: "new-project", Mode: proto.PeerWorkspaceCreatePlain,
	}, nil)
	require.NoError(t, err)
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())

	// Creating the same project again must fail rather than silently
	// reusing or overwriting the existing directory.
	_, err = server.createProject(t.Context(), proto.PeerWorkspaceCreateRequest{
		Root: root, RelativePath: "new-project", Mode: proto.PeerWorkspaceCreatePlain,
	}, nil)
	require.ErrorContains(t, err, "already exists")
}

func TestCreateProjectRejectsUnconfiguredRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))

	unconfigured, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	_, err = server.createProject(t.Context(), proto.PeerWorkspaceCreateRequest{
		Root: unconfigured, RelativePath: "project", Mode: proto.PeerWorkspaceCreatePlain,
	}, nil)
	require.ErrorContains(t, err, "not a configured workspace root")
	require.NoDirExists(t, filepath.Join(unconfigured, "project"))
}

func TestCreateProjectGitInitMode(t *testing.T) {
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))

	var lines []string
	path, err := server.createProject(t.Context(), proto.PeerWorkspaceCreateRequest{
		Root: root, RelativePath: "git-project", Mode: proto.PeerWorkspaceCreateGitInit,
	}, func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(path, ".git"))
}

func TestCreateProjectCloneModeRejectsInvalidURL(t *testing.T) {
	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, rootErr)
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))

	_, err := server.createProject(t.Context(), proto.PeerWorkspaceCreateRequest{
		Root: root, RelativePath: "cloned", Mode: proto.PeerWorkspaceCreateClone, CloneURL: "/local/path",
	}, nil)
	require.ErrorContains(t, err, "unsupported")
	require.NoDirExists(t, filepath.Join(root, "cloned"))
}

func TestCreateProjectCloneModeSurfacesUnreachableHostFailure(t *testing.T) {
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))

	// Port 0 on loopback with an explicit scheme passes URL validation but
	// can never accept a connection, so this exercises the full clone
	// dispatch path (validation, parent mkdir, git invocation, error
	// surfacing) without any real network dependency.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = server.createProject(ctx, proto.PeerWorkspaceCreateRequest{
		Root: root, RelativePath: "cloned", Mode: proto.PeerWorkspaceCreateClone, CloneURL: "http://127.0.0.1:1/repo.git",
	}, nil)
	require.Error(t, err)
}

// TestRunProjectCommandStreamsLocalGitClone exercises the real streaming and
// progress-reporting logic against an actual local git clone (a repository
// on local disk, not a network remote), verifying runProjectCommand itself
// works end to end without depending on network access.
func TestRunProjectCommandStreamsLocalGitClone(t *testing.T) {
	requireGit(t)
	source := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run(source, "init", "-q", "-b", "main")
	run(source, "config", "user.email", "test@example.com")
	run(source, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(source, "file.txt"), []byte("hello"), 0o644))
	run(source, "add", "file.txt")
	run(source, "commit", "-q", "-m", "initial")

	target := filepath.Join(t.TempDir(), "clone-target")
	var lines []string
	err := runProjectCommand(t.Context(), "", func(line string) { lines = append(lines, line) }, "git", "clone", "--", source, target)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(target, "file.txt"))
}

func TestRunProjectCommandReportsFailureContext(t *testing.T) {
	requireGit(t)
	err := runProjectCommand(t.Context(), t.TempDir(), nil, "git", "clone", "--", "not-a-valid-remote-url-at-all")
	require.Error(t, err)
	require.ErrorContains(t, err, "git failed")
}

func TestRunProjectCommandTimesOut(t *testing.T) {
	if _, lookErr := exec.LookPath("sleep"); lookErr != nil {
		t.Skip("sleep not available")
	}
	err := runProjectCommandWithTimeout(t.Context(), 50*time.Millisecond, "", nil, "sleep", "5")
	require.ErrorContains(t, err, "timed out")
}
