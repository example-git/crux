package providerplugin

func IsCanonicalMigratedProviderPreset(providerID, presetID, version string) bool {
	expectedID, expectedVersion, migrated := MigratedProviderPreset(providerID)
	return migrated && presetID == expectedID && version == expectedVersion
}

func IsCanonicalMigratedProviderPresetBundle(providerID, presetID, version, digest string) bool {
	expectedID, expectedVersion, expectedDigest, migrated := CanonicalMigratedProviderPreset(providerID)
	return migrated && presetID == expectedID && version == expectedVersion && equalDigest(digest, expectedDigest)
}

func applyTrustedRegistrationPolicy(status *Status) {
	if status.Trust != TrustTrusted {
		return
	}
	if status.preset != nil {
		providerID := string(status.preset.Preset.ID)
		if _, _, _, migrated := CanonicalMigratedProviderPreset(providerID); migrated &&
			!IsCanonicalMigratedProviderPresetBundle(providerID, status.preset.ID, status.preset.Version, status.Digest) {
			status.State = StateQuarantined
			status.Diagnostics = []Diagnostic{safeDiagnostic("migrated-preset-canonical-mismatch", "installed migrated provider preset does not match the canonical bundled version")}
			return
		}
	}
	status.State = StateRegistered
	status.Diagnostics = nil
}
