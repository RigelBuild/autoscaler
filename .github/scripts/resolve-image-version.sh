#!/usr/bin/env bash
# Return a validated release version from the tag or rehearsal input.
set -euo pipefail

event="$1" ref="$2" dispatch_version="$3" version_file="$4"
mapfile -t source_versions < <(sed -n 'p' "$version_file")
if [ "${#source_versions[@]}" -ne 1 ]; then
  echo "::error::expected exactly one version in ${version_file}, found ${#source_versions[@]}" >&2
  exit 1
fi
source_version="${source_versions[0]}"
case "$event" in
  push) version="${ref#refs/tags/v}" ;;
  workflow_dispatch) version="$dispatch_version" ;;
  *)
    echo "::error::unsupported event '${event}'" >&2
    exit 1
    ;;
esac

if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-rigel\.[1-9][0-9]*$ ]]; then
  echo "::error::version '${version}' does not match the required X.Y.Z-rigel.N shape" >&2
  exit 1
fi
if [ "$version" != "$source_version" ]; then
  echo "::error::version '${version}' does not match .github/rigel-version '${source_version}'" >&2
  exit 1
fi
printf '%s\n' "$version"
