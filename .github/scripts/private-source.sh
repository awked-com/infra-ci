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
  if ! (
    cd "$directory"
    env -i HOME="$HOME" PATH="$PATH" USER="${USER:-runner}" \
      TMPDIR="${RUNNER_TEMP:-/tmp}" GOTOOLCHAIN=local "$@"
  ) >"$log" 2>&1; then
    echo 'error: private source validation failed; reproduce checks locally' >&2
    exit 1
  fi
}

if [[ $operation == update ]]; then
  run nix flake update
  exit 0
fi
run go test -race ./...
run go vet ./...
run go build ./...
run nix flake check
