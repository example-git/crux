package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/example-git/crux/internal/fsext"
)

const snapshotRetention = 15

var ErrSnapshotOutsideWorkspace = errors.New("snapshot target is outside the workspace")

type externalSnapshotApprovalKey struct{}

func WithExternalSnapshotApproval(ctx context.Context) context.Context {
	return context.WithValue(ctx, externalSnapshotApprovalKey{}, true)
}

func externalSnapshotApproved(ctx context.Context) bool {
	approved, _ := ctx.Value(externalSnapshotApprovalKey{}).(bool)
	return approved
}

type snapshotEntry struct {
	Path     string       `json:"path"`
	Exists   bool         `json:"exists"`
	Mode     *fs.FileMode `json:"mode,omitempty"`
	Backup   string       `json:"backup,omitempty"`
	External bool         `json:"external,omitempty"`
}

type snapshotManifest struct {
	SessionID string                   `json:"session_id"`
	MessageID string                   `json:"message_id"`
	CreatedAt time.Time                `json:"created_at"`
	Files     map[string]snapshotEntry `json:"files"`
}

type snapshotBaseline struct {
	Path    string
	Content string
	Exists  bool
}

type snapshotStore struct {
	root       string
	workingDir string
	mu         sync.Mutex
}

func newSnapshotStore(root, workingDir string) (*snapshotStore, error) {
	canonical, err := fsext.CanonicalPath(workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot workspace: %w", err)
	}
	workspaceRoot := filepath.Join(root, digest(canonical))
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot root: %w", err)
	}
	return &snapshotStore{root: workspaceRoot, workingDir: canonical}, nil
}

func (s *snapshotStore) capture(ctx context.Context, sessionID, messageID, path, content string, exists bool, mode fs.FileMode) error {
	if sessionID == "" || messageID == "" {
		return nil
	}
	resolved, err := fsext.CanonicalPath(path)
	if err != nil {
		return fmt.Errorf("resolve snapshot target: %w", err)
	}
	external := !fsext.HasPrefix(resolved, s.workingDir)
	if external && !externalSnapshotApproved(ctx) {
		return fmt.Errorf("%w: %s", ErrSnapshotOutsideWorkspace, path)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	directory := s.messageDirectory(sessionID, messageID)
	manifest, err := s.loadManifest(directory)
	if err != nil {
		return err
	}
	if _, ok := manifest.Files[resolved]; ok {
		return nil
	}
	entry := snapshotEntry{Path: resolved, Exists: exists, External: external}
	if exists {
		mode := mode.Perm()
		entry.Mode = &mode
		entry.Backup = digest(resolved) + ".snapshot"
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create snapshot directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Backup), []byte(content), 0o600); err != nil {
			return fmt.Errorf("write file snapshot: %w", err)
		}
	}
	manifest.SessionID = sessionID
	manifest.MessageID = messageID
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now()
	}
	manifest.Files[resolved] = entry
	if err := s.writeManifest(directory, manifest); err != nil {
		return err
	}
	return s.cull(sessionID)
}

func (s *snapshotStore) rewind(_ context.Context, sessionID string, messageIDs []string, restore bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for index := len(messageIDs) - 1; index >= 0; index-- {
		directory := s.messageDirectory(sessionID, messageIDs[index])
		manifest, err := s.loadManifest(directory)
		if err != nil {
			return err
		}
		if restore {
			for _, entry := range manifest.Files {
				parent, err := fsext.CanonicalPath(filepath.Dir(entry.Path))
				if err != nil {
					return fmt.Errorf("resolve snapshot restore parent: %w", err)
				}
				resolved := filepath.Join(parent, filepath.Base(entry.Path))
				external := !fsext.HasPrefix(resolved, s.workingDir)
				if resolved != filepath.Clean(entry.Path) || external != entry.External {
					return fmt.Errorf("snapshot restore target is not authorized: %s", entry.Path)
				}
				info, statErr := os.Lstat(resolved)
				if statErr != nil && !os.IsNotExist(statErr) {
					return fmt.Errorf("inspect snapshot restore target: %w", statErr)
				}
				if !entry.Exists {
					if statErr == nil {
						if info.IsDir() {
							return fmt.Errorf("file created after checkpoint is now a directory: %s", entry.Path)
						}
						if err := os.Remove(resolved); err != nil {
							return fmt.Errorf("remove file created after checkpoint: %w", err)
						}
					}
					continue
				}
				if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
					if err := os.Remove(resolved); err != nil {
						return fmt.Errorf("remove symlink before file restore: %w", err)
					}
				} else if statErr == nil && info.IsDir() {
					return fmt.Errorf("snapshot restore target is now a directory: %s", entry.Path)
				}
				content, err := os.ReadFile(filepath.Join(directory, entry.Backup))
				if err != nil {
					return fmt.Errorf("read file snapshot: %w", err)
				}
				if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
					return fmt.Errorf("create restored file parent: %w", err)
				}
				if entry.Mode == nil {
					return fmt.Errorf("snapshot file mode is missing: %s", entry.Path)
				}
				if err := os.WriteFile(resolved, content, *entry.Mode); err != nil {
					return fmt.Errorf("restore file snapshot: %w", err)
				}
				if err := os.Chmod(resolved, *entry.Mode); err != nil {
					return fmt.Errorf("restore file snapshot mode: %w", err)
				}
			}
		}
	}
	for _, messageID := range messageIDs {
		if err := os.RemoveAll(s.messageDirectory(sessionID, messageID)); err != nil {
			return fmt.Errorf("remove restored checkpoint: %w", err)
		}
	}
	return nil
}

func (s *snapshotStore) loadManifest(directory string) (snapshotManifest, error) {
	manifest := snapshotManifest{Files: map[string]snapshotEntry{}}
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return manifest, nil
		}
		return snapshotManifest{}, fmt.Errorf("read snapshot manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return snapshotManifest{}, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.Files == nil {
		manifest.Files = map[string]snapshotEntry{}
	}
	return manifest, nil
}

func (s *snapshotStore) writeManifest(directory string, manifest snapshotManifest) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode snapshot manifest: %w", err)
	}
	temporary := filepath.Join(directory, "manifest.json.tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(directory, "manifest.json")); err != nil {
		return fmt.Errorf("replace snapshot manifest: %w", err)
	}
	return nil
}

func (s *snapshotStore) paths(sessionID, messageID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	manifest, err := s.loadManifest(s.messageDirectory(sessionID, messageID))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(manifest.Files))
	for path := range manifest.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *snapshotStore) baselines(sessionID, messageID string) ([]snapshotBaseline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	directory := s.messageDirectory(sessionID, messageID)
	manifest, err := s.loadManifest(directory)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(manifest.Files))
	for path := range manifest.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	baselines := make([]snapshotBaseline, 0, len(paths))
	for _, path := range paths {
		entry := manifest.Files[path]
		content := ""
		if entry.Exists {
			data, err := os.ReadFile(filepath.Join(directory, entry.Backup))
			if err != nil {
				return nil, fmt.Errorf("read file snapshot: %w", err)
			}
			content = string(data)
		}
		baselines = append(baselines, snapshotBaseline{Path: path, Content: content, Exists: entry.Exists})
	}
	return baselines, nil
}

func (s *snapshotStore) cull(sessionID string) error {
	directory := s.sessionDirectory(sessionID)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list snapshot checkpoints: %w", err)
	}
	type checkpoint struct {
		path      string
		createdAt time.Time
	}
	checkpoints := make([]checkpoint, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		manifest, err := s.loadManifest(path)
		if err != nil {
			return err
		}
		checkpoints = append(checkpoints, checkpoint{path: path, createdAt: manifest.CreatedAt})
	}
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].createdAt.After(checkpoints[j].createdAt) })
	if len(checkpoints) <= snapshotRetention {
		return nil
	}
	for _, checkpoint := range checkpoints[snapshotRetention:] {
		if err := os.RemoveAll(checkpoint.path); err != nil {
			return fmt.Errorf("cull snapshot checkpoint: %w", err)
		}
	}
	return nil
}

func (s *snapshotStore) deleteSession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.sessionDirectory(sessionID)); err != nil {
		return fmt.Errorf("remove session checkpoints: %w", err)
	}
	return nil
}

func (s *snapshotStore) sessionDirectory(sessionID string) string {
	return filepath.Join(s.root, digest(sessionID))
}

func (s *snapshotStore) messageDirectory(sessionID, messageID string) string {
	return filepath.Join(s.sessionDirectory(sessionID), digest(messageID))
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
