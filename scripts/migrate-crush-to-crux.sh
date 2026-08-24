#!/usr/bin/env bash
set -euo pipefail

if (( $# > 1 )); then
  printf 'usage: %s [project-directory]\n' "$0" >&2
  exit 2
fi

sources=()
targets=()

path_exists() {
  [[ -e "$1" || -L "$1" ]]
}

add_move() {
  local current_source=$1
  local current_target=$2
  local source=$3
  local target=$4
  local index

  if ! path_exists "$current_source"; then
    return
  fi
  if path_exists "$current_target"; then
    printf 'cannot migrate %s: target already exists: %s\n' "$current_source" "$current_target" >&2
    exit 1
  fi
  for (( index = 0; index < ${#sources[@]}; index++ )); do
    if [[ "${sources[index]}" == "$source" && "${targets[index]}" == "$target" ]]; then
      return
    fi
  done
  sources+=("$source")
  targets+=("$target")
}

home=${HOME:?HOME must be set}
config_home=${XDG_CONFIG_HOME:-$home/.ai-cli}
data_home=${XDG_DATA_HOME:-$home/.ai-cli/data}
cache_home=${XDG_CACHE_HOME:-$home/.cache}

legacy_config=$config_home/crush
crux_config=$config_home/crux
legacy_data=$data_home/crush
crux_data=$data_home/crux
legacy_cache=$cache_home/crush
crux_cache=$cache_home/crux

add_move "$legacy_config" "$crux_config" "$legacy_config" "$crux_config"
add_move "$legacy_config/crush.json" "$legacy_config/crux.json" "$crux_config/crush.json" "$crux_config/crux.json"
add_move "$legacy_config/crushrc" "$legacy_config/cruxrc" "$crux_config/crushrc" "$crux_config/cruxrc"

add_move "$legacy_data" "$crux_data" "$legacy_data" "$crux_data"
add_move "$legacy_data/crush.json" "$legacy_data/crux.json" "$crux_data/crush.json" "$crux_data/crux.json"

add_move "$legacy_cache" "$crux_cache" "$legacy_cache" "$crux_cache"
shopt -s nullglob
for legacy_log in "$legacy_cache"/server-*/crush.log; do
  relative_log=${legacy_log#"$legacy_cache"/}
  crux_log=$legacy_cache/${relative_log%crush.log}crux.log
  moved_log=$crux_cache/$relative_log
  moved_target=$crux_cache/${relative_log%crush.log}crux.log
  add_move "$legacy_log" "$crux_log" "$moved_log" "$moved_target"
done
shopt -u nullglob

if (( $# == 1 )); then
  if [[ ! -d "$1" ]]; then
    printf 'project directory does not exist: %s\n' "$1" >&2
    exit 1
  fi
  project=$(cd "$1" && pwd -P)
  if [[ ! -e "$project/.git" && ! -d "$project/.crush" && ! -e "$project/crush.json" && ! -e "$project/.crush.json" && ! -e "$project/crushrc" && ! -e "$project/.crushrc" ]]; then
    printf 'not a recognized project directory: %s\n' "$project" >&2
    exit 1
  fi

  legacy_project_data=$project/.crush
  crux_project_data=$project/.crux
  add_move "$legacy_project_data" "$crux_project_data" "$legacy_project_data" "$crux_project_data"
  add_move "$legacy_project_data/crush.json" "$legacy_project_data/crux.json" "$crux_project_data/crush.json" "$crux_project_data/crux.json"
  add_move "$legacy_project_data/crush.db" "$legacy_project_data/crux.db" "$crux_project_data/crush.db" "$crux_project_data/crux.db"
  add_move "$legacy_project_data/crush.lock" "$legacy_project_data/crux.lock" "$crux_project_data/crush.lock" "$crux_project_data/crux.lock"
  add_move "$legacy_project_data/logs/crush.log" "$legacy_project_data/logs/crux.log" "$crux_project_data/logs/crush.log" "$crux_project_data/logs/crux.log"
  add_move "$project/crush.json" "$project/crux.json" "$project/crush.json" "$project/crux.json"
  add_move "$project/.crush.json" "$project/.crux.json" "$project/.crush.json" "$project/.crux.json"
  add_move "$project/crushrc" "$project/cruxrc" "$project/crushrc" "$project/cruxrc"
  add_move "$project/.crushrc" "$project/.cruxrc" "$project/.crushrc" "$project/.cruxrc"
fi

if (( ${#sources[@]} == 0 )); then
  printf 'No default Crush directories or project files found.\n'
  exit 0
fi

for (( index = 0; index < ${#sources[@]}; index++ )); do
  source=${sources[index]}
  target=${targets[index]}
  mkdir -p "$(dirname "$target")"
  mv "$source" "$target"
  printf 'Moved %s -> %s\n' "$source" "$target"
done
