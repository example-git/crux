#!/bin/sh
set -eu

if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ]; then
    printf '%s\n' "GoReleaser matrix builds are forbidden on Apple Silicon. Run native validation locally and use the Linux GitHub Actions release workflows for packaging." >&2
    exit 1
fi
