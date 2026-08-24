package providerplugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/google/uuid"
)

// Install snapshots a local directory or materialized HTTPS Git tree into the
// canonical host-owned plugin directory. Source code is never run in place.
func (m *Manager) Install(ctx context.Context, request InstallRequest) (Snapshot, error) {
	source := strings.TrimSpace(request.Source)
	if source == "" {
		return Snapshot{}, errors.New("plugin source is required")
	}
	if strings.Contains(source, "://") {
		parsed, err := url.Parse(source)
		if err != nil || parsed.Scheme != "https" {
			return Snapshot{}, errors.New("remote plugin source must use HTTPS Git")
		}
		return m.installGit(ctx, request)
	}
	if request.Ref != "" {
		return Snapshot{}, errors.New("--ref is valid only for an HTTPS Git source")
	}
	return m.installDirectory(ctx, request, source)
}

func (m *Manager) installDirectory(ctx context.Context, request InstallRequest, source string) (Snapshot, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return Snapshot{}, errors.New("resolve local plugin source")
	}
	lockContext, cancel := context.WithTimeout(ctx, managerLockTimeout)
	defer cancel()
	release, err := lock.File(lockContext, m.paths.ManagerLock)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lock plugin installation: %w", err)
	}

	m.mu.Lock()
	if request.ExpectedRevision != 0 && request.ExpectedRevision != m.state.Revision {
		m.mu.Unlock()
		release()
		return Snapshot{}, ErrStaleRevision
	}
	staging := filepath.Join(m.paths.Bundles, ".install-"+uuid.NewString())
	snapshot, err := snapshotDirectory(absolute, staging)
	if err != nil {
		m.mu.Unlock()
		release()
		_ = os.RemoveAll(staging)
		return Snapshot{}, fmt.Errorf("snapshot local plugin source: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	validated, diagnostics := validateSnapshot(staging, snapshot)
	if len(diagnostics) > 0 {
		m.mu.Unlock()
		release()
		return Snapshot{}, fmt.Errorf("validate plugin bundle: %s", diagnostics[0].Message)
	}
	if diagnostics := compatibilityDiagnostics(validated.compatibility()); len(diagnostics) > 0 {
		m.mu.Unlock()
		release()
		return Snapshot{}, fmt.Errorf("plugin is incompatible: %s", diagnostics[0].Message)
	}
	provenance, err := loadProvenance(m.paths.ProvenanceFile)
	if err != nil {
		m.mu.Unlock()
		release()
		return Snapshot{}, err
	}
	sourceKind := request.sourceKind
	if sourceKind == "" {
		sourceKind = "local"
	}
	installedAt := time.Now().UTC()
	provenance.Records[validated.digest] = provenanceRecord{
		PluginID:   validated.id(),
		Digest:     validated.digest,
		SourceKind: sourceKind,
		Commit:     request.sourceCommit,
		Installed:  installedAt,
	}
	if err := saveProvenance(m.paths.ProvenanceFile, provenance); err != nil {
		m.mu.Unlock()
		release()
		return Snapshot{}, err
	}
	final := filepath.Join(m.paths.Bundles, validated.id()+bundleSuffix)
	if err := commitBundle(staging, final, request.Update); err != nil {
		m.mu.Unlock()
		release()
		return Snapshot{}, err
	}
	cleanup = false
	m.mu.Unlock()
	release()
	result, err := m.Rescan(ctx, 0)
	if err != nil || !request.Trust {
		return result, err
	}
	return m.SetTrust(ctx, validated.id(), TrustRequest{
		Digest:           validated.digest,
		Trusted:          true,
		ExpectedRevision: result.Revision,
	})
}

func commitBundle(staging, final string, update bool) error {
	info, err := os.Lstat(final)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, final); err != nil {
			return fmt.Errorf("commit plugin bundle: %w", err)
		}
		return syncDirectory(filepath.Dir(final))
	}
	if err != nil {
		return fmt.Errorf("inspect installed plugin: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("installed plugin destination is unsafe")
	}
	if !update {
		return errors.New("plugin is already installed; pass --update to replace it")
	}
	backup := final + ".rollback-" + uuid.NewString()
	if err := os.Rename(final, backup); err != nil {
		return fmt.Errorf("prepare plugin update: %w", err)
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.Rename(backup, final)
		return fmt.Errorf("commit plugin update: %w", err)
	}
	if err := syncDirectory(filepath.Dir(final)); err != nil {
		_ = os.RemoveAll(final)
		_ = os.Rename(backup, final)
		return fmt.Errorf("sync plugin update: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		_ = os.RemoveAll(final)
		if restoreErr := os.Rename(backup, final); restoreErr != nil {
			return fmt.Errorf("remove prior plugin generation: %w; restoring prior generation: %v", err, restoreErr)
		}
		_ = syncDirectory(filepath.Dir(final))
		return fmt.Errorf("remove prior plugin generation: %w", err)
	}
	return syncDirectory(filepath.Dir(final))
}
