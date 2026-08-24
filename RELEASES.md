# Crux Release Policy

## Channels

Crux uses GitHub releases as its only publication channel.

- Stable releases use annotated semantic-version tags such as `v1.2.3`.
- Prereleases use semantic-version prerelease tags such as `v1.2.3-rc.1`.
- Continuous nightly publication is disabled until Crux-owned infrastructure and demand justify it.

Release archives contain the `crux` executable, shell completions, manual page, license, third-party notices, schemas, and provider-plugin documentation where supported by the release configuration. Checksums are published with each release.

Crux does not publish through inherited Charm Homebrew, Scoop, AUR, NUR, npm, Gemfury, Winget, signing, or notarization infrastructure.

## Release process

1. Confirm the working tree contains only reviewed public material.
2. Run the full current-platform tests, race build, formatting, lint, generated-file consistency, dependency scan, secret scan, and identity-residue audit. Run the package snapshot only in the Linux GitHub Actions workflow.
3. Review license, attribution, security, and migration documentation.
4. Create and push the intended semantic-version tag.
5. Let the repository-local release workflow build and publish the GitHub release using only the repository `GITHUB_TOKEN`.
6. Verify archives, checksums, release notes, and installation instructions before announcement.

GoReleaser matrix builds are explicitly blocked on Apple Silicon because they perform unnecessary cross-platform compilation and create heavy local CPU and I/O load. Local validation is limited to the current platform; release packaging runs on Linux in GitHub Actions.

No release should require Charm-operated workflows, credentials, package registries, signing services, or support channels.
