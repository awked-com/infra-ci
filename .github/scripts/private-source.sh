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
    # Remove this diagnostic handoff after the Linux Go failure is identified
    # and the corrected maintenance check passes.
    if [[ $phase == 'go tests' && ${INFRA_GO_DIAGNOSTIC:-} == 1 && -n ${RUNNER_TEMP:-} ]]; then
      diagnostic="$RUNNER_TEMP/infra-go-failures.json"
      if ! jq -Rn '
        [inputs | fromjson? |
          select(type == "object" and .Action == "fail" and
            (.Package | type == "string") and
            (.Package | test("^[A-Za-z0-9_.@/-]{1,240}$")) and
            ((.Test == null) or
              ((.Test | type == "string") and (.Test | test("^[A-Za-z0-9_./-]{1,240}$"))))) |
          {package: .Package, test: (.Test // null)}] | unique | .[:20]
      ' "$log" >"$diagnostic" 2>/dev/null; then
        rm -f -- "$diagnostic"
      fi
    fi
    echo "error: private $phase failed; reproduce locally" >&2
    exit 1
  fi
}

if [[ $operation == update ]]; then
  run 'nix input update' nix flake update
  exit 0
fi
if [[ ${INFRA_GO_DIAGNOSTIC:-} == 1 ]]; then
  run 'go tests' nix shell --inputs-from . nixpkgs#sops --command go test -race -json ./...
else
  run 'go tests' nix shell --inputs-from . nixpkgs#sops --command go test -race ./...
fi
run 'go vet' go vet ./...
run 'go build' go build ./...
run 'nix flake check' nix flake check
