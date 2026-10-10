#!/usr/bin/env bash
# Check version selection, deterministic mint targets, collisions, and the non-tip probe.
set -u
script="$(cd "$(dirname "$0")" && pwd)/release-tag.sh"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
fails=0
mkdir -p "$dir/bin" "$dir/repo"
cat >"$dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"--method POST"*)
    printf '%s\n' "$*" >>"$GH_LOG"
    if [ "${GH_MODE:-create}" = collision ]; then
      : >"$GH_MARKER"
      echo 'Reference already exists' >&2
      echo 'HTTP 422' >&2
      exit 1
    fi
    ;;
  *"--method DELETE"*)
    printf '%s\n' "$*" >>"$GH_LOG"
    ;;
  *"git/ref/tags/"*)
    if [ "${GH_MODE:-create}" = existing ]; then exit 0; fi
    if [ "${GH_MODE:-create}" = collision ] && [ -e "$GH_MARKER" ]; then
      printf '%s\n' "$GH_EXISTING_SHA"
      exit 0
    fi
    exit 1
    ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
EOF
chmod +x "$dir/bin/gh"
export PATH="$dir/bin:$PATH"
export GH_LOG="$dir/gh.log"
export GH_MARKER="$dir/gh-marker"
cd "$dir/repo" || exit 1
git init -q -b rigel-release
git config user.name test
git config user.email test@example.invalid
write_version() {
  mkdir -p .github
  printf '%s\n' "$1" >.github/rigel-version
  git add .github/rigel-version
  git commit -q -m "$1"
  git rev-parse HEAD
}
old="$(write_version 1.5.0-rigel.1)"
base="$old"
new="$(write_version 1.5.0-rigel.2)"
noop="$(write_version 1.5.0-rigel.2)"
git tag v1.5.0-rigel.1 "$old"

expect_rc() {
  local name="$1" want="$2"; shift 2
  local rc
  "$@" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne "$want" ]; then
    echo "FAIL ${name}: rc=${rc} (want ${want})"
    fails=$((fails + 1))
  else
    echo "ok   ${name}"
  fi
}
"$script" check-pr "$base" "$new" >/dev/null 2>&1
if [ "$?" -eq 0 ]; then echo 'ok   next tracking version passes PR check'; else echo 'FAIL next tracking version PR check'; fails=$((fails + 1)); fi
skip="$(write_version 1.5.0-rigel.4)"
expect_rc 'skipped suffix fails PR check' 1 "$script" check-pr "$new" "$skip"
git checkout -q -b vendor-refresh "$new"
printf 'vendorHash = "refreshed";\n' >vendor.nix
git add vendor.nix
git commit -q -m vendor-refresh
noop="$(git rev-parse HEAD)"
expect_rc 'unchanged source passes PR check' 0 "$script" check-pr "$new" "$noop"

GH_MODE=create "$script" mint "$noop" RigelBuild/autoscaler >/dev/null 2>&1
if [ "$?" -eq 0 ] && grep -q 'refs/tags/v1.5.0-rigel.2' "$GH_LOG"; then
  echo 'ok   mint creates expected version ref'
else
  echo 'FAIL mint creates expected version ref'
  fails=$((fails + 1))
fi
if grep -q "sha=${new}" "$GH_LOG"; then
  echo 'ok   mint targets the commit that set the version'
else
  echo 'FAIL mint targets the commit that set the version'
  fails=$((fails + 1))
fi
before="$(wc -l <"$GH_LOG")"
GH_MODE=existing "$script" mint "$noop" RigelBuild/autoscaler >/dev/null 2>&1
if [ "$?" -eq 0 ] && [ "$(wc -l <"$GH_LOG")" -eq "$before" ]; then
  echo 'ok   existing remote release tag is a no-op'
else
  echo 'FAIL existing remote release tag is a no-op'
  fails=$((fails + 1))
fi
GH_MODE=collision GH_EXISTING_SHA="$new" "$script" mint "$new" RigelBuild/autoscaler >/dev/null 2>&1
if [ "$?" -eq 0 ]; then echo 'ok   same-target 422 succeeds'; else echo 'FAIL same-target 422 succeeds'; fails=$((fails + 1)); fi
rm -f "$GH_MARKER"
GH_MODE=collision GH_EXISTING_SHA="$old" "$script" mint "$new" RigelBuild/autoscaler >/dev/null 2>&1
if [ "$?" -ne 0 ]; then echo 'ok   conflicting 422 fails'; else echo 'FAIL conflicting 422 fails'; fails=$((fails + 1)); fi

git update-ref refs/remotes/origin/rigel-release "$noop"
git commit -q --allow-empty -m non-tip-release-tip
probe_tip="$(git rev-parse HEAD)"
git update-ref refs/remotes/origin/rigel-release "$probe_tip"
GITHUB_RUN_ID=probe-123 GH_MODE=create "$script" probe RigelBuild/autoscaler >/dev/null 2>&1
if [ "$?" -eq 0 ] && grep -q 'refs/tags/mint-probe-probe-123' "$GH_LOG" && grep -q -- '--method DELETE' "$GH_LOG"; then
  echo 'ok   non-tip probe creates and deletes its temporary tag'
else
  echo 'FAIL non-tip probe creates and deletes its temporary tag'
  fails=$((fails + 1))
fi

[ "$fails" -eq 0 ]
