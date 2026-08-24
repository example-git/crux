package backend

import (
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
)

// PluginSnapshot returns the authoritative redacted provider-plugin status for
// this execution host. Initialization is lazy so backends that never expose
// plugin status do not create host-global state as a constructor side effect.
func (b *Backend) PluginSnapshot() (proto.PluginSnapshot, error) {
	b.pluginOnce.Do(func() {
		b.plugins, b.pluginErr = providerplugin.NewManager(b.ctx, providerplugin.DefaultPaths(config.GlobalWorkspaceDir(), config.GlobalCacheDir()))
	})
	if b.pluginErr != nil || b.plugins == nil {
		return proto.PluginSnapshot{}, ErrPluginStatusUnavailable
	}
	profile, enabled, err := config.EffectiveProviderRollout()
	if err != nil {
		return proto.PluginSnapshot{}, err
	}
	return pluginSnapshotProto(b.plugins.Snapshot(), string(profile), enabled), nil
}

func pluginSnapshotProto(snapshot providerplugin.Snapshot, profile string, enabled []string) proto.PluginSnapshot {
	result := proto.PluginSnapshot{
		Profile:          profile,
		EnabledProviders: append([]string(nil), enabled...),
		Revision:         snapshot.Revision,
		ScannedAt:        snapshot.ScannedAt,
		Plugins:          make([]proto.PluginStatus, len(snapshot.Plugins)),
	}
	for i, status := range snapshot.Plugins {
		plugin := proto.PluginStatus{
			BundleName:    status.BundleName,
			ID:            status.ID,
			ProviderID:    status.ProviderID,
			Name:          status.Name,
			Version:       status.Version,
			PublisherID:   status.PublisherID,
			Digest:        status.Digest,
			State:         string(status.State),
			Trust:         string(status.Trust),
			Compatibility: string(status.Compatibility),
			SourceKind:    status.SourceKind,
			SourceCommit:  status.SourceCommit,
			Capabilities:  append([]string(nil), status.Capabilities...),
			InstalledAt:   status.InstalledAt,
		}
		plugin.Diagnostics = make([]proto.PluginDiagnostic, len(status.Diagnostics))
		for j, diagnostic := range status.Diagnostics {
			plugin.Diagnostics[j] = proto.PluginDiagnostic{
				Code:    diagnostic.Code,
				Message: diagnostic.Message,
			}
		}
		result.Plugins[i] = plugin
	}
	return result
}
