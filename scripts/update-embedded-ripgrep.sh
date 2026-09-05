#!/bin/sh
set -eu

version=15.2.0
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
destination="$root/internal/agent/tools/ripgrep"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/crux-ripgrep.XXXXXX")
trap 'rm -rf "$temporary"' EXIT

fetch() {
    url=$1
    output=$2
    if command -v curl >/dev/null 2>&1; then
        curl --fail --location --silent --show-error --output "$output" "$url"
    elif command -v python3 >/dev/null 2>&1; then
        python3 - "$url" "$output" <<'PY'
import shutil
import sys
import urllib.request

request = urllib.request.Request(sys.argv[1], headers={"User-Agent": "Crux-Asset-Updater"})
with urllib.request.urlopen(request, timeout=120) as response, open(sys.argv[2], "wb") as output:
    shutil.copyfileobj(response, output)
PY
    else
        printf '%s\n' "Updating embedded ripgrep requires curl or python3." >&2
        exit 1
    fi
}

checksum() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        printf '%s\n' "Updating embedded ripgrep requires shasum or sha256sum." >&2
        exit 1
    fi
}

mkdir -p "$destination"
while read -r target output expected; do
    archive="$temporary/ripgrep-$version-$target.tar.gz"
    fetch "https://github.com/BurntSushi/ripgrep/releases/download/$version/ripgrep-$version-$target.tar.gz" "$archive"
    actual=$(checksum "$archive")
    if [ "$actual" != "$expected" ]; then
        printf 'Checksum mismatch for %s: expected %s, got %s\n' "$target" "$expected" "$actual" >&2
        exit 1
    fi
    tar -xOf "$archive" "ripgrep-$version-$target/rg" > "$destination/$output"
    chmod 0644 "$destination/$output"
done <<'TARGETS'
aarch64-apple-darwin rg-darwin-arm64 3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4
x86_64-unknown-linux-musl rg-linux-amd64 33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c
aarch64-unknown-linux-musl rg-linux-arm64 800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915
TARGETS
