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

## Build and test

From the repository root, using Python 3.12 or newer with pip and venv:

```sh
python3 scripts/build-embedded-mitmproxy.py --target current
GOMAXPROCS=2 go test -race -p 2 -tags embedded_mitmproxy ./internal/trafficcapture -count=1 -timeout=5m
```

The tagged integration test needs tmux. It exercises the embedded proxy,
HTTPS trust, authenticated viewer access, capture output, and shutdown.
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
