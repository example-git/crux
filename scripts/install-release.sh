#!/usr/bin/env bash
set -euo pipefail

repo=example-git/crux

if (( $# > 1 )); then
  printf 'usage: %s [version]\n' "$0" >&2
  exit 2
fi

fetch() {
  local url=$1
  local output=$2
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --silent --show-error --output "$output" "$url"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$url" "$output" <<'PY'
import shutil
import sys
import urllib.request

request = urllib.request.Request(sys.argv[1], headers={"User-Agent": "Crux-Installer"})
with urllib.request.urlopen(request, timeout=120) as response, open(sys.argv[2], "wb") as output:
    shutil.copyfileobj(response, output)
PY
  else
    printf 'Crux installation requires curl or python3.\n' >&2
    exit 1
  fi
}

latest_version() {
  if command -v curl >/dev/null 2>&1; then
    local release_url
    release_url=$(curl --fail --location --silent --show-error --output /dev/null --write-out '%{url_effective}' "https://github.com/$repo/releases/latest")
    printf '%s\n' "${release_url##*/}"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$repo" <<'PY'
import sys
import urllib.request

request = urllib.request.Request(
    f"https://github.com/{sys.argv[1]}/releases/latest",
    headers={"User-Agent": "Crux-Installer"},
)
with urllib.request.urlopen(request, timeout=120) as response:
    print(response.geturl().rstrip("/").rsplit("/", 1)[-1])
PY
  else
    printf 'Crux installation requires curl or python3.\n' >&2
    exit 1
  fi
}

version=${1:-$(latest_version)}
version=${version#v}

case "$(uname -s)" in
  Darwin) release_os=Darwin ;;
  Linux) release_os=Linux ;;
  *)
    printf 'Unsupported operating system: %s\n' "$(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) release_arch=arm64 ;;
  x86_64|amd64) release_arch=x86_64 ;;
  *)
    printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2
    exit 1
    ;;
esac

asset=crux_${version}_${release_os}_${release_arch}.tar.gz
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/crux-install.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT

fetch "https://github.com/$repo/releases/download/v$version/$asset" "$temporary_dir/$asset"
tar -xzf "$temporary_dir/$asset" -C "$temporary_dir"

shopt -s nullglob
binaries=("$temporary_dir"/*/crux)
shopt -u nullglob
if (( ${#binaries[@]} != 1 )); then
  printf 'Expected one Crux binary in %s, found %d.\n' "$asset" "${#binaries[@]}" >&2
  exit 1
fi

install_dir=${HOME:?HOME must be set}/.ai-cli/bin
install_path=$install_dir/crux
mkdir -p "$install_dir"
rm -rf "$install_path"
cp "${binaries[0]}" "$install_path"
chmod 0755 "$install_path"
printf 'Installed Crux v%s to %s\n' "$version" "$install_path"
