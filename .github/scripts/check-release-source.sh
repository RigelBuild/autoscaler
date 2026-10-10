#!/usr/bin/env bash
# Fail unless the tag commit is reachable from the autoscaler release branch.
set -euo pipefail
ref="$1"
branch=rigel-release
commit="$(git rev-parse --verify "${ref}^{commit}")"

if ! git merge-base --is-ancestor "$commit" "origin/$branch" 2>/dev/null; then
  echo "::error::${commit} from ${ref} is not reachable from origin/${branch}" >&2
  exit 1
fi
printf '%s is reachable from origin/%s\n' "$commit" "$branch"
