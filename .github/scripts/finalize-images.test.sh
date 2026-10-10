#!/usr/bin/env bash
# Check digest-to-tag finalization with a deterministic Docker stub.
set -u
script="$(cd "$(dirname "$0")" && pwd)/finalize-images.sh"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
mkdir -p "$dir/bin"
cat >"$dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_LOG"
if [ "$1" = buildx ] && [ "$2" = imagetools ] && [ "$3" = create ] && [ -n "${FAIL_TAG:-}" ] && [ "$5" = "$FAIL_TAG" ]; then exit 1; fi
if [ "$1" = buildx ] && [ "$2" = imagetools ] && [ "$3" = inspect ]; then
  printf '%s\n' "$IMAGE_DIGEST"
fi
EOF
chmod +x "$dir/bin/docker"
export PATH="$dir/bin:$PATH"
export DOCKER_LOG="$dir/docker.log"
export GITHUB_STEP_SUMMARY="$dir/summary"
export VERSION=1.5.0-rigel.1
export COMMIT_SHA=0123456789abcdef0123456789abcdef01234567
export IMAGE_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
: >"$DOCKER_LOG"
: >"$GITHUB_STEP_SUMMARY"

if ! "$script"; then echo 'FAIL finalize succeeds for matching manifest digest'; exit 1; fi
[ "$(wc -l <"$DOCKER_LOG")" -eq 4 ] || { echo 'FAIL both tags are created and inspected'; exit 1; }
[ "$(wc -l <"$GITHUB_STEP_SUMMARY")" -eq 2 ] || { echo 'FAIL summary records both tag pins'; exit 1; }
grep -q "ghcr.io/rigelbuild/autoscaler:1.5.0-rigel.1@${IMAGE_DIGEST}" "$GITHUB_STEP_SUMMARY" || { echo 'FAIL version digest summary'; exit 1; }
grep -q "ghcr.io/rigelbuild/autoscaler:sha-${COMMIT_SHA}@${IMAGE_DIGEST}" "$GITHUB_STEP_SUMMARY" || { echo 'FAIL commit digest summary'; exit 1; }
grep -q "create --tag ghcr.io/rigelbuild/autoscaler:1.5.0-rigel.1" "$DOCKER_LOG" || { echo 'FAIL version tag is applied first'; exit 1; }
grep -q "create --tag ghcr.io/rigelbuild/autoscaler:sha-${COMMIT_SHA}" "$DOCKER_LOG" || { echo 'FAIL SHA tag is applied second'; exit 1; }
echo 'ok   both immutable digest tags appear in the summary'

: >"$DOCKER_LOG"
: >"$GITHUB_STEP_SUMMARY"
if FAIL_TAG="ghcr.io/rigelbuild/autoscaler:sha-${COMMIT_SHA}" "$script" >/dev/null 2>&1; then
  echo 'FAIL later tag failure propagates'
  exit 1
fi
[ ! -s "$GITHUB_STEP_SUMMARY" ] || { echo 'FAIL failed finalization leaves summary empty'; exit 1; }
echo 'ok   later tag failure propagates without a partial summary'
