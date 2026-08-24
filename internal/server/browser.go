package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/proto"
)

const maxBrowserEntries = 500

func (s *Server) allowedWorkspacePath(path string, requireDirectory bool) (string, string, error) {
	if path == "" {
		if len(s.workspaceRoots) == 0 {
			return "", "", fmt.Errorf("no workspace roots are configured")
		}
		path = s.workspaceRoots[0]
	}
	canonical, err := fsext.CanonicalPath(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	var root string
	for _, candidate := range s.workspaceRoots {
		if fsext.HasPrefix(canonical, candidate) && len(candidate) > len(root) {
			root = candidate
		}
	}
	if root == "" {
		return "", "", fmt.Errorf("path %q is outside configured workspace roots", path)
	}
	if requireDirectory {
		directory, openErr := openWorkspaceDirectory(root, canonical)
		if openErr != nil {
			return "", "", fmt.Errorf("access directory %q: %w", path, openErr)
		}
		if closeErr := directory.Close(); closeErr != nil {
			return "", "", fmt.Errorf("close directory %q: %w", path, closeErr)
		}
	}
	return canonical, root, nil
}

func (s *Server) browse(path string) (proto.BrowserListing, error) {
	canonical, root, err := s.allowedWorkspacePath(path, true)
	if err != nil {
		return proto.BrowserListing{}, err
	}
	directory, err := openWorkspaceDirectory(root, canonical)
	if err != nil {
		return proto.BrowserListing{}, fmt.Errorf("open directory %q: %w", canonical, err)
	}
	defer directory.Close()
	if err := validateOpenedWorkspaceDirectory(directory, canonical, root); err != nil {
		return proto.BrowserListing{}, err
	}
	entries, err := directory.ReadDir(maxBrowserEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return proto.BrowserListing{}, fmt.Errorf("read directory %q: %w", canonical, err)
	}
	truncated := len(entries) > maxBrowserEntries
	if truncated {
		entries = entries[:maxBrowserEntries]
	}
	result := make([]proto.BrowserEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			continue
		}
		result = append(result, proto.BrowserEntry{
			Name:      entry.Name(),
			Path:      filepath.Join(canonical, entry.Name()),
			Directory: entry.IsDir(),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Directory != result[j].Directory {
			return result[i].Directory
		}
		return result[i].Name < result[j].Name
	})
	parent := filepath.Dir(canonical)
	if canonical == root || !fsext.HasPrefix(parent, root) {
		parent = ""
	}
	return proto.BrowserListing{
		Roots:     append([]string(nil), s.workspaceRoots...),
		Path:      canonical,
		Parent:    parent,
		Entries:   result,
		Truncated: truncated,
	}, nil
}

func (s *Server) validateRemoteWorkspace(path, dataDir string) (string, string, error) {
	canonicalPath, _, err := s.allowedWorkspacePath(path, true)
	if err != nil {
		return "", "", err
	}
	if dataDir == "" {
		return canonicalPath, "", nil
	}
	canonicalDataDir, _, err := s.allowedWorkspacePath(dataDir, false)
	if err != nil {
		return "", "", fmt.Errorf("invalid data directory: %w", err)
	}
	return canonicalPath, canonicalDataDir, nil
}

func (c *controllerV1) handleGetBrowser(w http.ResponseWriter, r *http.Request) {
	if !c.server.authenticatedManagementRequest(r) {
		jsonError(w, http.StatusForbidden, "workspace browsing requires authenticated TLS")
		return
	}
	listing, err := c.server.browse(r.URL.Query().Get("path"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonEncode(w, listing)
}
