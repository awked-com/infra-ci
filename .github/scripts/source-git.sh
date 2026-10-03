#!/usr/bin/env bash
set +x
set -euo pipefail

operation=${1:-}
directory=${2:-}
branch=${3:-}

if [[ ! ${CI_SOURCE_REPOSITORY:-} =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
   [[ -z ${CI_SOURCE_TOKEN:-} ]] || [[ -z $directory ]]; then
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
cat >"$temporary/askpass" <<'SH'
#!/usr/bin/env bash
case "$1" in
  *Username*) printf '%s\n' x-access-token ;;
  *Password*) printf '%s\n' "$CI_SOURCE_TOKEN" ;;
  *) exit 1 ;;
esac
SH
chmod 700 "$temporary/askpass"
export GIT_ASKPASS="$temporary/askpass" GIT_TERMINAL_PROMPT=0

if [[ $operation == clone ]]; then
  if ! git -c credential.helper= clone --quiet --depth 1 --branch "$branch" \
    "https://github.com/${CI_SOURCE_REPOSITORY}.git" "$directory" >"$temporary/log" 2>&1; then
    echo 'error: private source checkout failed' >&2
    exit 1
  fi
else
  if ! git -C "$directory" -c credential.helper= -c core.hooksPath=/dev/null push --quiet \
    "https://github.com/${CI_SOURCE_REPOSITORY}.git" \
    "HEAD:refs/heads/$branch" >"$temporary/log" 2>&1; then
    echo 'error: update branch publication failed' >&2
    exit 1
  fi
fi
