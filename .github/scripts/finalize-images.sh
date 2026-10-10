#!/usr/bin/env bash
# Apply the release and commit tags to the immutable image digest.
set -euo pipefail

: "${VERSION:?VERSION is required}"
: "${COMMIT_SHA:?COMMIT_SHA is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

image=ghcr.io/rigelbuild/autoscaler
summary_lines=()
for tag in "$VERSION" "sha-${COMMIT_SHA}"; do
  docker buildx imagetools create --tag "${image}:${tag}" "${image}@${IMAGE_DIGEST}"
  reference="${image}:${tag}"
  resolved="$(docker buildx imagetools inspect "$reference" --format '{{.Manifest.Digest}}')"
  if [ "$resolved" != "$IMAGE_DIGEST" ]; then
    echo "::error::${reference} resolves to ${resolved}, expected ${IMAGE_DIGEST}" >&2
    exit 1
  fi
  summary_lines+=("${reference}@${resolved}")
done
printf '%s\n' "${summary_lines[@]}" >>"$GITHUB_STEP_SUMMARY"
