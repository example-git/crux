package config

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/redact"
)

// ProviderLoadIssue explains why an integration was left unloaded. It belongs
// to one runtime scan, never to the user's persisted provider configuration.
type ProviderLoadIssue struct {
	ProviderID string `json:"provider_id"`
	PluginID   string `json:"plugin_id,omitempty"`
	Version    string `json:"version,omitempty"`
	Message    string `json:"message"`
}

func (c *Config) providerLoadIssue(id string) *ProviderLoadIssue {
	for _, issue := range c.ProviderLoadIssues() {
		if issue.ProviderID == id {
			return &issue
		}
	}
	return nil
}

// A remote unloaded definition carries only retained selection metadata.
// No endpoint, configuration inputs, headers, or credentials can execute.
func unloadedProviderConfig(provider ProviderConfig) ProviderConfig {
	p := cloneProviderConfig(provider)
	return ProviderConfig{ID: p.ID, Name: p.Name, Type: p.Type, Owner: p.Owner, Plugin: p.Plugin, Preset: p.Preset, Models: p.Models, Disable: p.Disable}
}

func (c *Config) ProviderLoadIssues() []ProviderLoadIssue {
	if c == nil || c.providerScan == nil {
		return nil
	}
	return slices.Clone(c.providerScan.LoadIssues)
}

func (s *ProviderScan) rejectDelegatedBundle(providerID, pluginID, version string, err error) {
	message := redact.String(err.Error())
	s.LoadIssues = append(s.LoadIssues, ProviderLoadIssue{ProviderID: providerID, PluginID: pluginID, Version: version, Message: message})
	status := s.pluginStatuses[pluginID]
	status.State = providerplugin.StateIncompatible
	status.Compatibility = providerplugin.CompatibilityIncompatible
	status.Diagnostics = append(status.Diagnostics, providerplugin.Diagnostic{
		Code: "delegated-activation-failed", Message: message,
		Severity: providerplugin.DiagnosticSeverityError, Phase: providerplugin.DiagnosticPhaseActivation,
	})
	s.pluginStatuses[pluginID] = status
}

func (s *ProviderScan) collectMissingDelegatedProviders(cfg *Config) {
	if cfg == nil || cfg.Providers == nil {
		return
	}
	for _, id := range []string{"codex", "gemini-ag"} {
		provider, configured := cfg.Providers.Get(id)
		if !configured || slices.ContainsFunc(s.LoadIssues, func(issue ProviderLoadIssue) bool { return issue.ProviderID == id }) {
			continue
		}
		registration, active := s.Registry.Lookup(id)
		if active && registration.Manifest != nil && pluginReferenceMatches(provider.Plugin, registration) {
			continue
		}
		issue := ProviderLoadIssue{ProviderID: id, Message: "An active provider plugin bundle is required. Install or update the bundle and configure its plugin reference."}
		if provider.Plugin != nil {
			issue.PluginID, issue.Version = provider.Plugin.ID, provider.Plugin.Version
			if status, found := s.pluginStatuses[issue.PluginID]; found {
				issue.Message = fmt.Sprintf("Provider bundle is %s.", status.State)
				if status.State == providerplugin.StateRegistered {
					issue.Message = "The installed bundle is not enabled by the active provider profile or does not match the configured owner."
				}
				if len(status.Diagnostics) > 0 {
					issue.Message += " " + status.Diagnostics[0].Message
				}
				if active {
					issue.Message = fmt.Sprintf("Configured bundle version %s does not match active version %s.", issue.Version, registration.Manifest.Version)
				}
			}
		}
		issue.Message = redact.String(issue.Message)
		s.LoadIssues = append(s.LoadIssues, issue)
	}
	slices.SortFunc(s.LoadIssues, func(a, b ProviderLoadIssue) int { return cmp.Compare(a.ProviderID, b.ProviderID) })
}
