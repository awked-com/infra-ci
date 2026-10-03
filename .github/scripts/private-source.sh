#!/usr/bin/env bash
set +x
set -euo pipefail

operation=${1:-}
directory=${2:-}
if [[ $operation != check && $operation != update ]] || [[ ! -d $directory ]]; then
  echo 'error: invalid private source operation' >&2
  exit 1
fi

umask 077
log=$(mktemp)
trap 'rm -f -- "$log"' EXIT

run() {
  local phase=$1
  shift
  if ! (
    cd "$directory"
    env -i HOME="$HOME" PATH="$PATH" USER="${USER:-runner}" \
      TMPDIR="${RUNNER_TEMP:-/tmp}" GOTOOLCHAIN=local "$@"
  ) >"$log" 2>&1; then
    echo "error: private $phase failed; reproduce locally" >&2
    exit 1
  fi
}

if [[ $operation == update ]]; then
  run 'nix input update' nix flake update
  exit 0
fi
run 'go tests' nix shell --inputs-from . nixpkgs#sops --command go test -race ./...
run 'go vet' go vet ./...
run 'go build' go build ./...
run 'nix flake check' nix flake check
