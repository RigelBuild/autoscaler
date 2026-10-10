#!/usr/bin/env bash
# Check release ancestry against commits and tags in a scratch repository.
set -u
script="$(cd "$(dirname "$0")" && pwd)/check-release-source.sh"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
fails=0
cd "$dir" || exit 1
git init -q -b rigel-release
git config user.name test
git config user.email test@example.invalid
git commit -q --allow-empty -m release-base
base="$(git rev-parse HEAD)"
git commit -q --allow-empty -m release-tip
tip="$(git rev-parse HEAD)"
git tag v1.5.0-rigel.1 "$base"
git checkout -q --orphan unrelated
git commit -q --allow-empty -m unrelated
unrelated="$(git rev-parse HEAD)"
git update-ref refs/remotes/origin/rigel-release "$tip"

expect() {
  local name="$1" want_rc="$2" ref="$3" rc
  "$script" "$ref" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL $name: rc=$rc (want $want_rc)"
    fails=$((fails + 1))
  else
    echo "ok   $name"
  fi
}

expect 'release branch commit passes' 0 "$tip"
expect 'release tag commit passes' 0 refs/tags/v1.5.0-rigel.1
expect 'unrelated branch commit fails' 1 "$unrelated"
expect 'missing ref fails' 128 missing-ref
git update-ref -d refs/remotes/origin/rigel-release
expect 'missing release branch fails' 1 "$tip"

[ "$fails" -eq 0 ]
