package providerplugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/pubsub"
)

// SetTrust approves or revokes one exact currently discovered digest. Trust is
// never granted by plugin ID, source path, or publisher alone.
func (m *Manager) SetTrust(ctx context.Context, pluginID string, request TrustRequest) (Snapshot, error) {
	if pluginID == "" || len(request.Digest) != 64 {
		return Snapshot{}, errors.New("plugin ID and exact SHA-256 digest are required")
	}
	lockContext, cancel := context.WithTimeout(ctx, managerLockTimeout)
	defer cancel()
	release, err := lock.File(lockContext, m.paths.ManagerLock)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lock plugin trust state: %w", err)
	}
	defer release()

	m.mu.Lock()
	defer m.mu.Unlock()
	if request.ExpectedRevision != 0 && request.ExpectedRevision != m.state.Revision {
		return Snapshot{}, ErrStaleRevision
	}
	index := -1
	for i := range m.state.Plugins {
		status := m.state.Plugins[i]
		if status.ID == pluginID && equalDigest(status.Digest, request.Digest) {
			index = i
			break
		}
	}
	if index < 0 {
		return Snapshot{}, ErrPluginMissing
	}
	status := m.state.Plugins[index]
	if (status.manifest == nil && status.preset == nil) || status.Compatibility != CompatibilityCompatible || status.State == StateInvalid || status.State == StateIncompatible {
		return Snapshot{}, errors.New("plugin is not eligible for trust")
	}
	store, err := loadTrustStore(m.paths.TrustFile)
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	record := trustRecord{
		PluginID:    status.ID,
		ProviderID:  status.ProviderID,
		PublisherID: status.PublisherID,
		Digest:      status.Digest,
		ApprovedAt:  now,
	}
	if !request.Trusted {
		record.RevokedAt = &now
	}
	store.Records[status.Digest] = record
	if err := saveTrustStore(m.paths.TrustFile, store); err != nil {
		return Snapshot{}, err
	}
	m.trust = store
	if request.Trusted {
		revokedOnly := status.Trust == TrustRevoked && status.State == StateQuarantined &&
			len(status.Diagnostics) == 1 && status.Diagnostics[0].Code == "trust-revoked"
		m.state.Plugins[index].Trust = TrustTrusted
		if m.state.Plugins[index].State == StateUntrusted || revokedOnly {
			m.state.Plugins[index].State = StateRegistered
			m.state.Plugins[index].Diagnostics = nil
		}
	} else {
		m.state.Plugins[index].Trust = TrustRevoked
		m.state.Plugins[index].State = StateQuarantined
		m.state.Plugins[index].Diagnostics = []Diagnostic{safeDiagnostic("trust-revoked", "trust for this exact plugin digest was revoked")}
	}
	m.state.Revision++
	m.state.ScannedAt = now
	result := m.state.clone()
	m.events.Publish(pubsub.UpdatedEvent, Event{Revision: result.Revision, Kind: "trust-changed", ChangedIDs: []string{pluginID}})
	return result, nil
}
