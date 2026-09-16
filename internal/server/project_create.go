package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/proto"
)

// projectCreateTimeout bounds how long a single git operation (init or
// clone) may run before the request is abandoned.
const projectCreateTimeout = 5 * time.Minute

// scpLikeCloneURL matches the SCP-style git remote syntax (user@host:path)
// in addition to the http(s):// and ssh:// forms handled explicitly below.
var scpLikeCloneURL = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:[^\s]+$`)

// createProject creates a new project directory under one of the server's
// configured workspace roots, optionally initializing or populating it with
// git, and returns the resulting canonical path suitable for opening as a
// workspace via the existing CreateWorkspace flow. progress receives
// best-effort output lines for long-running modes (git clone) and may be
// nil.
func (s *Server) createProject(ctx context.Context, request proto.PeerWorkspaceCreateRequest, progress func(string)) (string, error) {
	target, err := s.resolveProjectTarget(request.Root, request.RelativePath)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(target); statErr == nil {
		return "", fmt.Errorf("path %q already exists", target)
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("check path %q: %w", target, statErr)
	}

	switch request.Mode {
	case proto.PeerWorkspaceCreatePlain, proto.PeerWorkspaceCreateGitInit:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", fmt.Errorf("create directory %q: %w", target, err)
		}
		if request.Mode == proto.PeerWorkspaceCreateGitInit {
			if err := runProjectCommand(ctx, target, progress, "git", "init"); err != nil {
				return "", err
			}
		}
	case proto.PeerWorkspaceCreateClone:
		if err := validateCloneURL(request.CloneURL); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("create parent directory: %w", err)
		}
		// "--" prevents a hostile clone URL from being interpreted as a
		// git option even though validateCloneURL already rejects any
		// value that starts with "-".
		if err := runProjectCommand(ctx, "", progress, "git", "clone", "--", request.CloneURL, target); err != nil {
			return "", err
		}
	default:
		return "", errors.New("unsupported project creation mode")
	}

	canonical, _, err := s.allowedWorkspacePath(target, true)
	if err != nil {
		return "", err
	}
	return canonical, nil
}

// resolveProjectTarget validates that root is one of the server's
// configured workspace roots and that relativePath is a safe, traversal-free
// path under it, returning the absolute target directory. The directory
// itself need not exist yet.
func (s *Server) resolveProjectTarget(root, relativePath string) (string, error) {
	var matchedRoot string
	for _, candidate := range s.workspaceRoots {
		if candidate == root {
			matchedRoot = candidate
			break
		}
	}
	if matchedRoot == "" {
		return "", fmt.Errorf("root %q is not a configured workspace root", root)
	}
	cleaned := filepath.ToSlash(filepath.Clean("/" + relativePath))
	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	for _, segment := range segments {
		if !validProjectSegment(segment) {
			return "", fmt.Errorf("invalid project directory name %q", segment)
		}
	}
	target := filepath.Join(append([]string{matchedRoot}, segments...)...)
	if !fsext.HasPrefix(target, matchedRoot) {
		return "", errors.New("project path escapes the configured workspace root")
	}
	return target, nil
}

func validProjectSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	if strings.ContainsAny(segment, "/\\\x00") {
		return false
	}
	if strings.TrimSpace(segment) != segment {
		return false
	}
	return utf8.ValidString(segment)
}

// validateCloneURL rejects anything that is not a well-formed http(s)://,
// ssh://, or SCP-like (user@host:path) git remote. A leading "-" is always
// rejected so a hostile URL cannot be interpreted as a command-line option
// by git even before the "--" argument separator is considered.
func validateCloneURL(raw string) error {
	if raw == "" {
		return errors.New("clone URL is required")
	}
	if strings.HasPrefix(raw, "-") {
		return errors.New("invalid clone URL")
	}
	switch {
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "ssh://"):
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return errors.New("invalid clone URL")
		}
		return nil
	case scpLikeCloneURL.MatchString(raw):
		return nil
	default:
		return errors.New("unsupported clone URL scheme")
	}
}

// runProjectCommand runs an external command with the standard
// projectCreateTimeout bound, streaming combined stdout/stderr lines to
// progress (which may be nil). Arguments are always passed as argv, never
// through a shell.
func runProjectCommand(ctx context.Context, dir string, progress func(string), name string, args ...string) error {
	return runProjectCommandWithTimeout(ctx, projectCreateTimeout, dir, progress, name, args...)
}

// runProjectCommandWithTimeout is runProjectCommand with an explicit
// timeout, split out so tests can exercise the timeout path quickly instead
// of waiting on the production-sized projectCreateTimeout.
func runProjectCommandWithTimeout(ctx context.Context, timeout time.Duration, dir string, progress func(string), name string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	done := make(chan struct{})
	var lastLines []string
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 4096), 64<<10)
		for scanner.Scan() {
			line := scanner.Text()
			if progress != nil {
				progress(line)
			}
			lastLines = append(lastLines, line)
			if len(lastLines) > 20 {
				lastLines = lastLines[len(lastLines)-20:]
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		<-done
		return fmt.Errorf("start %s: %w", name, err)
	}
	waitErr := cmd.Wait()
	_ = writer.Close()
	<-done
	if waitErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s timed out", name)
		}
		return fmt.Errorf("%s failed: %w: %s", name, waitErr, strings.Join(lastLines, "\n"))
	}
	return nil
}
