package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
)

type protectedShellAccess struct {
	mu          sync.Mutex
	denied      error
	translator  *pathtranslate.Translator
	permissions permission.Service
	sessionID   string
	callID      string
	paths       map[string]struct{}
	reports     map[*os.File][]byte
	reportError error
}

func shellPath(cwd, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	canonical, err := fsext.CanonicalPath(path)
	if err == nil {
		return canonical
	}
	return filepath.Clean(path)
}

func (p *protectedShellAccess) Command(ctx context.Context, cwd string, args []string) error {
	p.mu.Lock()
	denied := p.denied
	p.mu.Unlock()
	if denied != nil {
		return denied
	}
	if len(args) == 0 {
		return nil
	}
	command := filepath.Base(args[0])
	if command == "command" || command == "exec" || command == "sudo" || command == "env" {
		for index := 1; index < len(args); index++ {
			if !strings.HasPrefix(args[index], "-") && !strings.Contains(args[index], "=") {
				if err := p.Command(ctx, cwd, args[index:]); err != nil {
					return err
				}
				break
			}
		}
	}
	var paths []string
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			if _, value, ok := strings.Cut(arg, "="); ok && value != "" {
				paths = append(paths, shellPath(cwd, value))
			}
			continue
		}
		paths = append(paths, shellPath(cwd, arg))
	}
	p.recordPaths(append(paths, cwd)...)
	if command == "mv" || command == "rename" {
		for _, path := range paths {
			if err := p.translator.CheckMove(path, path); err != nil {
				return err
			}
		}
	}
	for _, path := range append(paths, cwd, shellPath(cwd, args[0])) {
		if err := p.translator.CheckInternalPath(path); err != nil {
			return p.deny(err)
		}
	}
	for _, path := range append(paths, cwd, shellPath(cwd, args[0])) {
		if p.translator.Protected(path) {
			return p.approve(ctx, path, args)
		}
	}
	return nil
}

func (p *protectedShellAccess) File(ctx context.Context, cwd, path string) error {
	path = shellPath(cwd, path)
	p.recordPaths(cwd, path)
	if err := p.translator.CheckInternalPath(path); err != nil {
		return p.deny(err)
	}
	if !p.translator.Protected(path) {
		return nil
	}
	return p.approve(ctx, path, nil)
}

func (p *protectedShellAccess) deny(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = err
	return err
}

func (p *protectedShellAccess) approve(ctx context.Context, path string, args []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied != nil {
		return p.denied
	}
	if p.permissions == nil {
		return errors.New("protected shell access requires permission service")
	}
	approved, err := p.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID: p.sessionID, ToolCallID: p.callID, ToolName: BashToolName,
		Action: "protected_path", Path: path, RequireExplicitApproval: true, IgnoreSkipRequests: true,
		Description: fmt.Sprintf("Allow shell access to protected path: %s", path),
		Params:      map[string]any{"path": path, "command": args},
	})
	if err != nil {
		return err
	}
	if !approved {
		p.denied = errors.New("shell access to protected path was not approved")
		return p.denied
	}
	return nil
}

func checkProtectedWorkspaceMoves(ctx context.Context, edit *protocol.WorkspaceEdit) error {
	translator := pathtranslate.FromContext(ctx)
	if translator == nil {
		return nil
	}
	for _, change := range edit.DocumentChanges {
		if change.RenameFile == nil {
			continue
		}
		source, err := change.RenameFile.OldURI.Path()
		if err != nil {
			return err
		}
		destination, err := change.RenameFile.NewURI.Path()
		if err != nil {
			return err
		}
		if err := translator.CheckMove(source, destination); err != nil {
			return err
		}
	}
	return nil
}

func protectedShellContext(ctx context.Context, permissions permission.Service, callID string) context.Context {
	if translator := pathtranslate.FromContext(ctx); translator != nil {
		return shell.WithAccessFilter(ctx, &protectedShellAccess{translator: translator, permissions: permissions, sessionID: GetSessionFromContext(ctx), callID: callID})
	}
	return ctx
}

func (p *protectedShellAccess) recordPaths(paths ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordPathsLocked(paths...)
}

func (p *protectedShellAccess) recordPathsLocked(paths ...string) {
	if p.paths == nil {
		p.paths = make(map[string]struct{})
	}
	for _, path := range paths {
		if filepath.IsAbs(path) {
			p.paths[path] = struct{}{}
		}
	}
}

func (p *protectedShellAccess) OutputPaths() []string {
	paths, _ := p.ReadOutputPaths()
	return paths
}

func (p *protectedShellAccess) ReadOutputPaths() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for file := range p.reports {
		if err := p.readDirectoryReportLocked(file); err != nil {
			p.reportError = err
		}
	}
	if p.reportError != nil {
		return nil, p.reportError
	}
	paths := make([]string, 0, len(p.paths))
	for path := range p.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func (p *protectedShellAccess) NativeDirectoryReport() (string, func() error, error) {
	if p.translator.Fingerprint() == "" {
		return "", func() error { return nil }, nil
	}
	file, err := os.CreateTemp("", "crux-native-directories-*")
	if err != nil {
		return "", nil, err
	}
	p.mu.Lock()
	if p.reports == nil {
		p.reports = make(map[*os.File][]byte)
	}
	p.reports[file] = nil
	p.mu.Unlock()
	finish := func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if _, active := p.reports[file]; !active {
			return nil
		}
		err := p.readDirectoryReportLocked(file)
		if err == nil && len(p.reports[file]) > 0 {
			err = errors.New("incomplete native shell directory report")
		}
		delete(p.reports, file)
		err = errors.Join(err, file.Close(), os.Remove(file.Name()))
		if err != nil {
			p.reportError = err
		}
		return err
	}
	return file.Name(), finish, nil
}

func (p *protectedShellAccess) readDirectoryReportLocked(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	remaining := info.Size() - offset
	var buffer [4096]byte
	for remaining > 0 {
		count, err := file.Read(buffer[:min(int64(len(buffer)), remaining)])
		remaining -= int64(count)
		pending := append(p.reports[file], buffer[:count]...)
		for {
			end := bytes.IndexByte(pending, 0)
			if end < 0 {
				break
			}
			path := string(pending[:end])
			if !filepath.IsAbs(path) {
				return errors.New("native shell reported a non-absolute working directory")
			}
			p.recordPathsLocked(shellPath("", path))
			pending = pending[end+1:]
		}
		p.reports[file] = pending
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func withShellOutputPaths(ctx context.Context, response fantasy.ToolResponse, paths []string, structured bool) (fantasy.ToolResponse, error) {
	if len(paths) == 0 {
		return response, nil
	}
	metadata := make(map[string]json.RawMessage)
	if response.Metadata != "" {
		if err := json.Unmarshal([]byte(response.Metadata), &metadata); err != nil {
			return response, err
		}
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		return response, err
	}
	metadata["shell_output_paths"] = encoded
	if translator := pathtranslate.FromContext(ctx); translator != nil {
		var references []pathtranslate.PathReference
		if structured {
			references, err = translator.OutputJSONReferences(response.Content, paths)
		} else {
			references, err = translator.OutputReferences(response.Content, paths)
		}
		if err != nil {
			return response, err
		}
		encoded, err = json.Marshal(references)
		if err != nil {
			return response, err
		}
		metadata["path_references"] = encoded
	}
	encoded, err = json.Marshal(metadata)
	response.Metadata = string(encoded)
	return response, err
}
