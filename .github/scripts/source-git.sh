#!/usr/bin/env bash
set +x
set -euo pipefail

operation=${1:-}
directory=${2:-}
branch=${3:-}

if [[ ! ${CI_SOURCE_REPOSITORY:-} =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
   [[ -z ${CI_DEPLOY_KEY:-} ]] || [[ -z $directory ]]; then
  echo 'error: private Git access is not configured' >&2
  exit 1
fi
if [[ $operation != clone && $operation != push ]]; then
  echo 'error: invalid private Git operation' >&2
  exit 1
fi
if [[ $operation == clone && -z $branch ]]; then branch=main; fi
if [[ $branch != main && ! $branch =~ ^codex/nix-inputs-[0-9]+-[0-9]+$ ]] ||
   [[ $operation != clone && $branch == main ]]; then
  echo 'error: invalid update branch' >&2
  exit 1
fi

umask 077
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
printf '%s\n' "$CI_DEPLOY_KEY" >"$temporary/key"
printf '%s\n' 'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' >"$temporary/known_hosts"
export GIT_SSH_COMMAND="ssh -F /dev/null -i \"$temporary/key\" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=\"$temporary/known_hosts\""

if [[ $operation == clone ]]; then
  if ! git clone --quiet --depth 1 --branch "$branch" \
    "git@github.com:${CI_SOURCE_REPOSITORY}.git" "$directory" >"$temporary/log" 2>&1; then
    echo 'error: private source checkout failed' >&2
    exit 1
  fi
else
  if ! git -C "$directory" -c core.hooksPath=/dev/null push --quiet origin \
    "HEAD:refs/heads/$branch" >"$temporary/log" 2>&1; then
    echo 'error: update branch publication failed' >&2
    exit 1
  fi
fi
