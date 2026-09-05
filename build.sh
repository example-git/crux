#!/usr/bin/env bash
set -uo pipefail

run_step() {
  local label=$1
  shift

  printf '==> %s...\n' "$label"
  if "$@"; then
    printf 'SUCCESS: %s.\n' "$label"
    return 0
  else
    local status=$?
    printf 'FAILURE: %s exited with status %d.\n' "$label" "$status" >&2
    return "$status"
  fi
}

embedded_mitmproxy=0
go_tag_args=()

build_crux() {
  CGO_ENABLED=0 GOEXPERIMENT=greenteagc go build -v ${go_tag_args[@]+"${go_tag_args[@]}"} -o crux .
}

build_crux_amd64() {
  GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 GOEXPERIMENT=greenteagc go build -v -o crux-amd64 . || return
  arch -x86_64 ./crux-amd64 --version >/dev/null
}

backup_crux() {
  local backup_dir=$HOME/.ai-cli/backups/crux
  local timestamp
  timestamp=$(date -u +%Y%m%dT%H%M%SZ)
  local backup=$backup_dir/crux-$timestamp-$$

  mkdir -p "$backup_dir"
  cp -p "$HOME/.ai-cli/bin/crux" "$backup"
  printf 'Backup saved to %s\n' "$backup"
}

install_crux() {
  mkdir -p "$HOME/.ai-cli/bin"
  # removes the bin because if a process is running with the old bin you cant swap in place,
  # that new bin wont launch so we gotta delete the one that has locks on it before swapping
  # in the new one.
  rm -f "$HOME/.ai-cli/bin/crux"
  cp -f crux "$HOME/.ai-cli/bin/crux"
  chmod 0755 "$HOME/.ai-cli/bin/crux"
}

run_build() {
  run_step "Building Crux" build_crux
}

run_amd64_build() {
  if [[ $(uname -s) != Darwin || $(uname -m) != arm64 ]]; then
    printf 'FAILURE: --build-amd64 requires Apple Silicon macOS with Rosetta.\n' >&2
    return 2
  fi
  if ! arch -x86_64 /usr/bin/true 2>/dev/null; then
    printf 'FAILURE: Rosetta is not installed or unavailable.\n' >&2
    return 2
  fi
  run_step "Building and verifying the macOS amd64 Crux binary with Rosetta" build_crux_amd64
}

run_install() {
  run_build || return
  if [[ -e "$HOME/.ai-cli/bin/crux" ]]; then
    run_step "Backing up the existing Crux binary outside PATH" backup_crux || return
  fi
  run_step "Installing Crux to $HOME/.ai-cli/bin/crux" install_crux
}

run_tests() {
  run_step "Running the full race test suite" go test -race -failfast ${go_tag_args[@]+"${go_tag_args[@]}"} ./...
}

run_checks() {
  run_step "Building all packages with the race detector" go build -race ${go_tag_args[@]+"${go_tag_args[@]}"} ./... &&
    run_step "Checking log capitalization" ./scripts/check_log_capitalization.sh
}

usage() {
  printf 'Usage: %s [MODE] [--embedded-mitmproxy]\n' "$0"
  printf '  --build               Build ./crux without installing it\n'
  printf '  --build-amd64         Build ./crux-amd64 for macOS amd64 and verify it with Rosetta\n'
  printf '  --install             Build and install Crux (default)\n'
  printf '  --test                Run the full race test suite\n'
  printf '  --check               Run the race build and log capitalization check\n'
  printf '  --all                 Run tests, checks, build, and install\n'
  printf '  --embedded-mitmproxy  Embed the in-process mitmproxy runtime\n'
}

mode=
for argument in "$@"; do
  case "$argument" in
    --embedded-mitmproxy)
      embedded_mitmproxy=1
      ;;
    --build|--build-amd64|--install|--test|--check|--all|--help|-h)
      if [[ -n $mode ]]; then
        printf 'FAILURE: multiple modes requested: %s and %s.\n' "$mode" "$argument" >&2
        usage >&2
        exit 2
      fi
      mode=$argument
      ;;
    *)
      printf 'FAILURE: unknown mode %s.\n' "$argument" >&2
      usage >&2
      exit 2
      ;;
  esac
done
mode=${mode:---install}

if (( embedded_mitmproxy )) && [[ $mode != --help && $mode != -h ]]; then
  if [[ $mode == --build-amd64 ]]; then
    printf 'FAILURE: embedded mitmproxy supports Darwin arm64, Linux amd64, and Linux arm64 only.\n' >&2
    exit 2
  fi
  run_step "Preparing the embedded mitmproxy runtime" python3 ./scripts/build-embedded-mitmproxy.py --target current || exit
  go_tag_args=(-tags embedded_mitmproxy)
fi

case "$mode" in
  --build)
    run_build
    ;;
  --build-amd64)
    run_amd64_build
    ;;
  --install)
    run_install
    ;;
  --test)
    run_tests
    ;;
  --check)
    run_checks
    ;;
  --all)
    run_tests && run_checks && run_install
    ;;
  --help|-h)
    usage
    ;;
esac
