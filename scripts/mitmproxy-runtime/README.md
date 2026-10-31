# Embedded mitmproxy runtime

The runtime uses upstream mitmproxy `13.0.0.dev0` at revision
`b506c68108e287104045333ade476d92c39c275e`, not a stable release. This revision
permits the security-fixed dependencies that the published 12.2.3 release
excludes. The builder pins and verifies the source archive SHA-256 before
building a wheel in an isolated environment with hashed build dependencies. The
builder removes the terminal-console package and its unconditional Urwid dependency
before creating a web-only wheel. Crux embeds and invokes mitmweb, not the terminal UI.

Runtime dependency locks select cryptography 50.0.0, h2 4.4.1, Tornado 6.5.8,
and pyOpenSSL 26.4.0. Upstream no longer needs msgpack and uses backports.zstd
instead of zstandard. All runtime dependencies are installed from hashed
binary wheels. The source revision, source checksum, build-lock checksum, and
target-lock checksum are recorded in `CRUX_RUNTIME.json` for cache invalidation.

The exact backports.zstd 1.7.0 wheels pinned for all three targets declare
`PSF-2.0` and preserve the Python and BSD zstd license files in their wheel metadata.
GitHub dependency review currently reports a broader license expression for this
package, so the workflow excludes only `pkg:pypi/backports.zstd@1.7.0` from license
classification. Package versions and wheel contents remain constrained by the target
lock hashes. Urwid is not excepted and is excluded from the web-only runtime.

## Capture-local TLS behavior

Capture deliberately relaxes TLS verification without changing host Keychains or
uncaptured processes. The embedded proxy accepts upstream certificates, Node uses
`NODE_TLS_REJECT_UNAUTHORIZED=0`, and Git uses `GIT_SSL_NO_VERIFY=true`. A private,
session-local Python `sitecustomize` hook makes the default HTTPS context
unverified while chaining any existing customization. Session CA bundles remain
available through standard client environment variables. Explicit `unset_env`
entries take precedence over these settings.

This is not a universal certificate-pinning bypass. Go/macOS-native clients that
ignore environment-provided roots, explicitly configured TLS contexts, and Python
`-I`/`-S` startup need application-specific support. Captured processes must not be
used for unrelated sensitive browsing or transactions.

## Build and test

From the repository root, using Python 3.12 or newer with pip and venv:

```sh
python3 scripts/build-embedded-mitmproxy.py --target current
GOMAXPROCS=2 go test -race -p 2 -tags embedded_mitmproxy ./internal/trafficcapture -count=1 -timeout=5m
```

The native per-app integration tests require macOS, tmux, and xcrun with a Swift
compiler and macOS SDK. They build ad-hoc-signed capture apps and may request
approval for their scoped VPN configurations. The tests exercise the embedded
proxy, HTTPS trust and capture-local TLS relaxation, the authenticated viewer,
management-shell interaction, capture output, and shutdown. Enforcement tests
verify that direct parent/helper requests appear in mitmproxy and cannot bypass
capture after WireGuard server loss or packet-provider termination, while an
uncaptured control remains connected. A public-endpoint diagnostic also needs
Internet access. Test policies are removed during cleanup.
Only native runtime execution is validated by this command. Linux lock metadata
and wheel availability do not establish Linux runtime compatibility.

## Updating the pin

Update the exact source revision, source archive SHA-256, and package version
in `scripts/build-embedded-mitmproxy.py`, plus the runtime identity in
`internal/trafficcapture/runtime.go`. Inspect the selected revision's package
metadata before choosing dependency versions.

Resolve the source wheel and dependency pins, download binary wheels for Python
3.12 and each target platform declared by the builder, and regenerate the
SHA-256 lock entries from those wheel files. Check dependency markers against
each target's environment, including platform-specific mitmproxy-rs packages. Keep the web-only source patch
aligned with upstream metadata and verify that web startup does not import console
modules before changing the mitmproxy revision.
Keep `requirements-linux-amd64-no-hashes.txt` aligned with its hashed lock.
Build tools are pinned separately in `requirements-build.txt`; they are not
shipped in the embedded runtime.

Rebuild and run the native tagged test before using the new archive. Existing
archives with different manifest pins are rebuilt automatically. Do not bypass
upstream dependency constraints or hash verification to force an update.
