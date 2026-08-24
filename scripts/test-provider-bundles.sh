#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

printf 'Testing provider host contracts...\n'
go test \
  ./foundation/providers/anthropic \
  ./foundation/providers/openai \
  ./foundation/providers/openaicompat \
  ./internal/providertransport/... \
  ./internal/providerplugin/manifest \
  ./internal/providerplugin \
  ./internal/providerregistry \
  ./internal/agent

printf 'Validating provider bundles...\n'
if (($# == 0)); then
  go run ./scripts/validate_provider_bundles.go
else
  go run ./scripts/validate_provider_bundles.go "$@"
fi
