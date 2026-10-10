#!/usr/bin/env bash
# Check tag and dispatch versions against the release version file.
set -u
script="$(dirname "$0")/resolve-image-version.sh"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
printf '%s\n' '1.5.0-rigel.1' >"$dir/rigel-version"
fails=0

expect() {
  local name="$1" want_rc="$2" want_out="$3"
  shift 3
  local out rc
  out="$("$script" "$@" 2>/dev/null)"
  rc=$?
  if [ "$rc" != "$want_rc" ] || [ "$out" != "$want_out" ]; then
    echo "FAIL $name: rc=$rc out='$out' (want rc=$want_rc out='$want_out')"
    fails=$((fails + 1))
  else
    echo "ok   $name"
  fi
}

expect 'tag push strips v' 0 1.5.0-rigel.1 push refs/tags/v1.5.0-rigel.1 '' "$dir/rigel-version"
expect 'dispatch uses input' 0 1.5.0-rigel.1 workflow_dispatch refs/heads/rigel-release 1.5.0-rigel.1 "$dir/rigel-version"
expect 'tag mismatch fails' 1 '' push refs/tags/v1.5.0-rigel.2 '' "$dir/rigel-version"
expect 'dispatch mismatch fails' 1 '' workflow_dispatch refs/heads/rigel-release 1.5.0-rigel.2 "$dir/rigel-version"
expect 'empty dispatch input fails' 1 '' workflow_dispatch refs/heads/rigel-release '' "$dir/rigel-version"
expect 'leading zero version fails' 1 '' workflow_dispatch refs/heads/rigel-release 01.5.0-rigel.1 "$dir/rigel-version"
expect 'zero rigel suffix fails' 1 '' workflow_dispatch refs/heads/rigel-release 1.5.0-rigel.0 "$dir/rigel-version"
expect 'unsupported event fails' 1 '' pull_request refs/heads/rigel-release '' "$dir/rigel-version"
printf '%s\n' '1.5.0-rigel.1' '1.5.0-rigel.2' >"$dir/rigel-version"
expect 'multiple version lines fail' 1 '' push refs/tags/v1.5.0-rigel.1 '' "$dir/rigel-version"
printf '%s\n' '1.5.0-rigel.1' >"$dir/rigel-version"
expect 'missing version source fails' 1 '' push refs/tags/v1.5.0-rigel.1 '' "$dir/missing"

[ "$fails" -eq 0 ]
