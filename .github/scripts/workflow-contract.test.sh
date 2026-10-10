#!/usr/bin/env bash
# Static acceptance checks for image-publisher Requirement 5 and release-tag Requirement 1.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
publisher="$root/.github/workflows/publish-image.yaml"
release="$root/.github/workflows/release-tag.yaml"
fail() { echo "FAIL $1" >&2; exit 1; }
pass() { echo "ok   $1"; }
expect() {
  local name="$1" actual="$2" expected="$3"
  [ "$actual" = "$expected" ] || fail "$name (got '$actual', expected '$expected')"
}

[ -f "$publisher" ] || fail 'publish-image.yaml exists'
[ -f "$release" ] || fail 'release-tag.yaml exists'
[ ! -e "$root/.github/workflows/publish-image.yml" ] || fail 'old publisher extension removed'
if grep -Eq '^[[:space:]]*on:' "$publisher" "$release"; then fail 'workflow on key is quoted'; fi
expect 'publisher event count' "$(yq -r '.on | keys | length' "$publisher")" 2
expect 'publisher tag trigger' "$(yq -r '.on.push.tags[0]' "$publisher")" 'v*.*.*-rigel.*'
expect 'dispatch version required' "$(yq -r '.on.workflow_dispatch.inputs.version.required' "$publisher")" true
expect 'concurrency group is per tag ref' "$(yq -r '.concurrency.group' "$publisher")" '${{ github.workflow }}-${{ github.ref }}'
expect 'concurrency does not cancel' "$(yq -r '.concurrency["cancel-in-progress"]' "$publisher")" false
expect 'publish depends on resolve' "$(yq -r '.jobs.publish.needs' "$publisher")" resolve
expect 'rehearsal depends on resolve' "$(yq -r '.jobs.rehearsal.needs' "$publisher")" resolve
expect 'publish is push only' "$(yq -r '.jobs.publish.if' "$publisher")" "github.event_name == 'push'"
expect 'rehearsal is dispatch only' "$(yq -r '.jobs.rehearsal.if' "$publisher")" "github.event_name == 'workflow_dispatch'"
expect 'finalize is push only' "$(yq -r '.jobs.finalize.if' "$publisher")" "github.event_name == 'push'"
expect 'publish alone gets package write' "$(yq -r '.jobs.publish.permissions.packages' "$publisher")" write
expect 'rehearsal has no package write' "$(yq -r '.jobs.rehearsal.permissions.packages // "read"' "$publisher")" read
expect 'finalize has contents write' "$(yq -r '.jobs.finalize.permissions.contents' "$publisher")" write
expect 'publisher has only resolve guard before work' "$(yq -r '.jobs.resolve.steps | map(select(.name == "Require release ancestry")) | length' "$publisher")" 1
expect 'publisher fetches release history' "$(yq -r '.jobs.resolve.steps | map(select(.name == "Checkout")) | .[0].with.fetch-depth' "$publisher")" 0
expect 'resolve uses release branch for ancestry' "$(yq -r '.jobs.resolve.steps | map(select(.name == "Require release ancestry")) | .[0].env.REF' "$publisher")" '${{ github.ref }}'
expect 'resolve checks the version source' "$(yq -r '.jobs.resolve.steps | map(select(.name == "Resolve and validate version")) | length' "$publisher")" 1
contains_version_file="$(grep -F -c '.github/rigel-version' "$root/.github/scripts/resolve-image-version.sh")"
expect 'version resolver reads the required source' "$contains_version_file" 1
expect 'finalize creates the version tag' "$(grep -F -c '"${image}:${tag}" "${image}@${IMAGE_DIGEST}"' "$root/.github/scripts/finalize-images.sh")" 1
expect 'finalize verifies resolved manifest digest' "$(grep -F -c '[ "$resolved" != "$IMAGE_DIGEST" ]' "$root/.github/scripts/finalize-images.sh")" 1
[[ "$(cat "$root/.github/rigel-version")" =~ ^[0-9]+\.[0-9]+\.[0-9]+-rigel\.[1-9][0-9]*$ ]] || fail 'publisher version source is X.Y.Z-rigel.N'
expect 'release source is widened for build args' "$(grep -F -c 'ARG TARGETOS TARGETARCH CI_COMMIT_SHA CI_COMMIT_TAG CI_COMMIT_BRANCH' "$root/docker/Dockerfile")" 1
expect 'PR CI runs release static tests' "$(grep -F -c '.github/scripts/workflow-contract.test.sh' "$root/.woodpecker/test.yaml")" 1
expect 'tag publish uses upstream Dockerfile' "$(yq -r '.jobs.publish.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.file' "$publisher")" docker/Dockerfile
expect 'tag publish is multi-arch' "$(yq -r '.jobs.publish.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.platforms' "$publisher")" 'linux/amd64,linux/arm64'
expect 'dispatch rehearsal is multi-arch' "$(yq -r '.jobs.rehearsal.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.platforms' "$publisher")" 'linux/amd64,linux/arm64'
expect 'tag publish emits provenance' "$(yq -r '.jobs.publish.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.provenance' "$publisher")" 'mode=max'
expect 'dispatch rehearsal does not push' "$(yq -r '.jobs.rehearsal.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.push' "$publisher")" false
expect 'rehearsal has no registry login' "$(yq -r '.jobs.rehearsal.steps | map(select(.uses | test("docker/login-action"))) | length' "$publisher")" 0
expect 'rehearsal has no registry output' "$(yq -r '.jobs.rehearsal.steps | map(select(.uses | test("docker/build-push-action"))) | .[0].with.outputs // "none"' "$publisher")" none
expect 'finalize waits for release digest' "$(yq -r '.jobs.finalize.needs[]' "$publisher" | sort | paste -sd, -)" 'publish,resolve'
expect 'finalize runs the image tag script' "$(yq -r '.jobs.finalize.steps | map(select(.name == "Apply release tags and record manifest digest")) | length' "$publisher")" 1
# A job that runs a repo script must check out before it; a bare runner has no tree.
for wf in "$publisher" "$release"; do
  for job in $(yq -r '.jobs | keys | .[]' "$wf"); do
    first_script="$(yq -r ".jobs.\"$job\".steps | to_entries | map(select(.value.run // \"\" | test(\"\\.github/scripts/\"))) | .[0].key // \"\"" "$wf")"
    [ -n "$first_script" ] || continue
    first_checkout="$(yq -r ".jobs.\"$job\".steps | to_entries | map(select(.value.uses // \"\" | test(\"^actions/checkout@\"))) | .[0].key // \"\"" "$wf")"
    [ -n "$first_checkout" ] && [ "$first_checkout" -lt "$first_script" ] || fail "$job checks out before running repo scripts"
    pass "$job checks out before running repo scripts"
  done
done
expect 'release is finalized last' "$(yq -r '.jobs.finalize.steps[-1].name' "$publisher")" 'Create GitHub Release'
expect 'no QEMU action' "$(grep -c 'docker/setup-qemu-action' "$publisher" || true)" 0
pass 'image-publisher Requirement 5 static contract'

expect 'release pull requests target rigel-release' "$(yq -r '.on.pull_request.branches[0]' "$release")" rigel-release
expect 'release pushes target rigel-release' "$(yq -r '.on.push.branches[0]' "$release")" rigel-release
expect 'mint workflow supports dispatch' "$(yq -r '.on | has("workflow_dispatch")' "$release")" true
expect 'check job is pull-request only' "$(yq -r '.jobs.check.if' "$release")" "github.event_name == 'pull_request'"
expect 'mint excludes pull requests' "$(yq -r '.jobs.mint.if' "$release")" "github.event_name != 'pull_request'"
expect 'check has contents read only' "$(yq -r '.jobs.check.permissions | keys | join(",")' "$release")" contents
expect 'workflow token has contents read only' "$(yq -r '.permissions | keys | join(",")' "$release")" contents
expect 'check validates changed version source' "$(yq -r '.jobs.check.steps | map(select(.name == "Validate a changed release version")) | length' "$release")" 1
expect 'mint uses release environment' "$(yq -r '.jobs.mint.environment' "$release")" rigel-release
expect 'dispatch guard exists before App token step' "$(yq -r '.jobs.mint.steps | map(select(.name == "Require release-branch dispatch")) | length' "$release")" 1
expect 'mint checks out full history' "$(yq -r '.jobs.mint.steps | map(select(.name == "Checkout")) | .[0].with.fetch-depth' "$release")" 0
expect 'mint takes an App token on every run' "$(yq -r '.jobs.mint.steps | map(select(.uses | test("actions/create-github-app-token"))) | length' "$release")" 1
expect 'mint uses the E1 client ID' "$(yq -r '.jobs.mint.steps | map(select(.uses | test("actions/create-github-app-token"))) | .[0].with.client-id' "$release")" '${{ vars.AUTOMATION_APP_CLIENT_ID }}'
expect 'mint uses the E1 private key' "$(yq -r '.jobs.mint.steps | map(select(.uses | test("actions/create-github-app-token"))) | .[0].with.private-key' "$release")" '${{ secrets.AUTOMATION_APP_PRIVATE_KEY }}'
expect 'mint token has contents write' "$(yq -r '.jobs.mint.steps | map(select(.uses | test("actions/create-github-app-token"))) | .[0].with.permission-contents' "$release")" write
expect 'mint token is repository scoped' "$(yq -r '.jobs.mint.steps | map(select(.uses | test("actions/create-github-app-token"))) | .[0].with.repositories' "$release")" autoscaler
expect 'probe input exists' "$(yq -r '.on.workflow_dispatch.inputs.probe.type' "$release")" boolean
expect 'mint has no concurrency' "$(yq -r '.jobs.mint.concurrency // "none"' "$release")" none
while IFS= read -r line; do
  [[ "$line" =~ uses:[[:space:]]+[^[:space:]#]+@[0-9a-f]{40}[[:space:]]+#\ v[^[:space:]]+[[:space:]]*$ ]] || fail "third-party action is not SHA-pinned with a version trailer: $line"
done < <(grep -h 'uses:' "$publisher" "$release")
pass 'release-tagging Requirement 1 static contract and SHA-pinned actions'
