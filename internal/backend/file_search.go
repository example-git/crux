package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
	"github.com/sahilm/fuzzy"
)

var ErrInvalidFileSearch = errors.New("invalid workspace file search")

type workspaceFileSearchCandidate struct {
	root      string
	path      string
	matchType string
}

func (b *Backend) SearchWorkspaceFiles(ctx context.Context, workspaceID string, request proto.WorkspaceFileSearchRequest) (proto.WorkspaceFileSearchResponse, error) {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.WorkspaceFileSearchResponse{}, err
	}
	if request.Limit <= 0 || request.Limit > 1000 {
		return proto.WorkspaceFileSearchResponse{}, fmt.Errorf("%w: limit must be between 1 and 1000", ErrInvalidFileSearch)
	}
	roots, err := normalizeWorkspaceFileSearchRoots(workspace.resolvedPath, request.Roots)
	if err != nil {
		return proto.WorkspaceFileSearchResponse{}, err
	}
	response := proto.WorkspaceFileSearchResponse{
		Roots: roots,
		Files: make([]proto.WorkspaceFileSearchMatch, 0),
	}
	if request.Query == "" || len(roots) == 0 {
		return response, nil
	}

	candidates, err := collectWorkspaceFileSearchCandidates(ctx, roots)
	if err != nil {
		return proto.WorkspaceFileSearchResponse{}, err
	}
	response.Files, err = matchWorkspaceFileSearchCandidates(ctx, request.Query, candidates, request.Limit)
	if err != nil {
		return proto.WorkspaceFileSearchResponse{}, err
	}
	return response, nil
}

func collectWorkspaceFileSearchCandidates(ctx context.Context, roots []string) ([]workspaceFileSearchCandidate, error) {
	candidates := make([]workspaceFileSearchCandidate, 0)
	seen := make(map[string]struct{})
	for _, root := range roots {
		paths, _, walkErr := fsext.GlobGitignoreAwareCtx(ctx, "**/*", root, 0)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if walkErr != nil {
			continue
		}
		for _, path := range paths {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			info, statErr := os.Lstat(path)
			if statErr != nil {
				continue
			}
			matchType := "file"
			if info.IsDir() {
				matchType = "directory"
			}
			relative = filepath.ToSlash(relative)
			key := root + "\x00" + matchType + "\x00" + relative
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, workspaceFileSearchCandidate{root: root, path: relative, matchType: matchType})
		}
	}
	return candidates, nil
}

func matchWorkspaceFileSearchCandidates(ctx context.Context, query string, candidates []workspaceFileSearchCandidate, limit int) ([]proto.WorkspaceFileSearchMatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := make([]string, len(candidates))
	for index := range candidates {
		paths[index] = candidates[index].path
	}
	matches := fuzzy.Find(query, paths)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scoreOffset := 0
	for _, match := range matches {
		if match.Score+scoreOffset < 0 {
			scoreOffset = -match.Score
		}
	}
	files := make([]proto.WorkspaceFileSearchMatch, 0, min(len(matches), limit))
	for _, match := range matches {
		candidate := candidates[match.Index]
		indices := make([]uint32, len(match.MatchedIndexes))
		for index, matchedIndex := range match.MatchedIndexes {
			indices[index] = uint32(matchedIndex)
		}
		files = append(files, proto.WorkspaceFileSearchMatch{
			Root:      candidate.root,
			Path:      candidate.path,
			MatchType: candidate.matchType,
			FileName:  filepath.Base(candidate.path),
			Score:     uint32(match.Score + scoreOffset),
			Indices:   indices,
		})
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Score == files[j].Score {
			return files[i].Path < files[j].Path
		}
		return files[i].Score > files[j].Score
	})
	if len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

func normalizeWorkspaceFileSearchRoots(workspaceRoot string, roots []string) ([]string, error) {
	if workspaceRoot == "" {
		return nil, fmt.Errorf("%w: workspace root is unavailable", ErrInvalidFileSearch)
	}
	workspaceRoot, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFileSearch, err)
	}
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(workspaceRoot); resolveErr == nil {
		workspaceRoot = resolvedRoot
	}
	normalized := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			return nil, fmt.Errorf("%w: fuzzy file search root must not be empty", ErrInvalidFileSearch)
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(workspaceRoot, root)
		}
		root, err = filepath.Abs(filepath.Clean(root))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidFileSearch, err)
		}
		if resolvedRoot, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = resolvedRoot
		}
		relative, relErr := filepath.Rel(workspaceRoot, root)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: fuzzy file search root is outside the workspace: %s", ErrInvalidFileSearch, root)
		}
		normalized = append(normalized, root)
	}
	return normalized, nil
}

type workspaceFileSearchSession struct {
	workspaceID string
	roots       []string
	candidates  []workspaceFileSearchCandidate
	query       string
	limit       int
	updateID    string
	ctx         context.Context
	cancel      context.CancelFunc
}

func workspaceFileSearchSessionKey(owner, sessionID string) string {
	return owner + "\x00" + sessionID
}

func validateWorkspaceFileSearchSession(owner, sessionID string) error {
	if owner == "" {
		return fmt.Errorf("%w: fuzzy file search owner must not be empty", ErrInvalidFileSearch)
	}
	if sessionID == "" {
		return fmt.Errorf("%w: sessionId must not be empty", ErrInvalidFileSearch)
	}
	return nil
}

func (b *Backend) StartWorkspaceFileSearchSession(ctx context.Context, workspaceID string, request proto.WorkspaceFileSearchSessionStartRequest) error {
	if err := validateWorkspaceFileSearchSession(request.Owner, request.SessionID); err != nil {
		return err
	}
	if len(request.Roots) == 0 {
		return fmt.Errorf("%w: at least one search root is required", ErrInvalidFileSearch)
	}
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}
	roots, err := normalizeWorkspaceFileSearchRoots(workspace.resolvedPath, request.Roots)
	if err != nil {
		return err
	}
	candidates, err := collectWorkspaceFileSearchCandidates(ctx, roots)
	if err != nil {
		return err
	}
	key := workspaceFileSearchSessionKey(request.Owner, request.SessionID)
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	if b.fileSearch == nil {
		b.fileSearch = make(map[string]*workspaceFileSearchSession)
	}
	if previous := b.fileSearch[key]; previous != nil && previous.cancel != nil {
		previous.cancel()
	}
	b.fileSearch[key] = &workspaceFileSearchSession{
		workspaceID: workspaceID,
		roots:       append([]string(nil), roots...),
		candidates:  candidates,
	}
	return nil
}

func (b *Backend) BeginWorkspaceFileSearchSessionUpdate(workspaceID string, request proto.WorkspaceFileSearchSessionBeginRequest) (proto.WorkspaceFileSearchSessionBeginResponse, error) {
	if err := validateWorkspaceFileSearchSession(request.Owner, request.SessionID); err != nil {
		return proto.WorkspaceFileSearchSessionBeginResponse{}, err
	}
	if request.Limit <= 0 || request.Limit > 1000 {
		return proto.WorkspaceFileSearchSessionBeginResponse{}, fmt.Errorf("%w: limit must be between 1 and 1000", ErrInvalidFileSearch)
	}
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.WorkspaceFileSearchSessionBeginResponse{}, err
	}
	key := workspaceFileSearchSessionKey(request.Owner, request.SessionID)
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	session := b.fileSearch[key]
	if session == nil || session.workspaceID != workspaceID {
		return proto.WorkspaceFileSearchSessionBeginResponse{}, fmt.Errorf("%w: fuzzy file search session not found: %s", ErrInvalidFileSearch, request.SessionID)
	}
	if session.cancel != nil {
		session.cancel()
	}
	parent := workspace.ctx
	if parent == nil {
		parent = b.ctx
	}
	if parent == nil {
		parent = context.Background()
	}
	session.ctx, session.cancel = context.WithCancel(parent)
	session.query = request.Query
	session.limit = request.Limit
	session.updateID = uuid.NewString()
	return proto.WorkspaceFileSearchSessionBeginResponse{UpdateID: session.updateID}, nil
}

func (b *Backend) CompleteWorkspaceFileSearchSessionUpdate(workspaceID string, request proto.WorkspaceFileSearchSessionCompleteRequest) (proto.WorkspaceFileSearchSessionCompleteResponse, error) {
	if err := validateWorkspaceFileSearchSession(request.Owner, request.SessionID); err != nil {
		return proto.WorkspaceFileSearchSessionCompleteResponse{}, err
	}
	key := workspaceFileSearchSessionKey(request.Owner, request.SessionID)
	b.fileSearchMu.Lock()
	session := b.fileSearch[key]
	if session == nil || session.workspaceID != workspaceID || session.updateID != request.UpdateID {
		b.fileSearchMu.Unlock()
		return proto.WorkspaceFileSearchSessionCompleteResponse{Files: []proto.WorkspaceFileSearchMatch{}}, nil
	}
	searchContext := session.ctx
	query := session.query
	limit := session.limit
	candidates := session.candidates
	b.fileSearchMu.Unlock()

	files, searchErr := matchWorkspaceFileSearchCandidates(searchContext, query, candidates, limit)
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	session = b.fileSearch[key]
	if session == nil || session.workspaceID != workspaceID || session.updateID != request.UpdateID {
		return proto.WorkspaceFileSearchSessionCompleteResponse{Files: []proto.WorkspaceFileSearchMatch{}}, nil
	}
	session.cancel = nil
	session.updateID = ""
	response := proto.WorkspaceFileSearchSessionCompleteResponse{Accepted: true, Query: query, Files: files}
	if response.Files == nil {
		response.Files = []proto.WorkspaceFileSearchMatch{}
	}
	if searchErr != nil {
		response.Files = []proto.WorkspaceFileSearchMatch{}
		response.Error = searchErr.Error()
	}
	return response, nil
}

func (b *Backend) StopWorkspaceFileSearchSession(workspaceID string, request proto.WorkspaceFileSearchSessionRequest) error {
	if err := validateWorkspaceFileSearchSession(request.Owner, request.SessionID); err != nil {
		return err
	}
	key := workspaceFileSearchSessionKey(request.Owner, request.SessionID)
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	session := b.fileSearch[key]
	if session == nil || session.workspaceID != workspaceID {
		return nil
	}
	if session.cancel != nil {
		session.cancel()
	}
	delete(b.fileSearch, key)
	return nil
}

func (b *Backend) closeWorkspaceFileSearchSessions(workspaceID string) {
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	for key, session := range b.fileSearch {
		if session.workspaceID != workspaceID {
			continue
		}
		if session.cancel != nil {
			session.cancel()
		}
		delete(b.fileSearch, key)
	}
}

func (b *Backend) closeAllWorkspaceFileSearchSessions() {
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	for key, session := range b.fileSearch {
		if session.cancel != nil {
			session.cancel()
		}
		delete(b.fileSearch, key)
	}
}

func (b *Backend) CloseWorkspaceFileSearchOwner(owner string) error {
	if owner == "" {
		return fmt.Errorf("%w: fuzzy file search owner must not be empty", ErrInvalidFileSearch)
	}
	prefix := owner + "\x00"
	b.fileSearchMu.Lock()
	defer b.fileSearchMu.Unlock()
	for key, session := range b.fileSearch {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if session.cancel != nil {
			session.cancel()
		}
		delete(b.fileSearch, key)
	}
	return nil
}
