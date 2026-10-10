#!/usr/bin/env bash
# Validate autoscaler version changes or mint their deterministic release tag.
set -euo pipefail

readonly VERSION_RE='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-rigel\.([1-9][0-9]*)$'
export LC_ALL=C

usage() {
  echo 'Usage: release-tag.sh check-pr <base-sha> <head-sha> | mint <sha> <owner/repo>' >&2
  exit 2
}

version_at() {
  local commit="$1" required="${2:-yes}" versions=()
  mapfile -t versions < <(git show "${commit}:.github/rigel-version" 2>/dev/null | sed -n 'p')
  if [ "${#versions[@]}" -ne 1 ]; then
    if [ "$required" = yes ]; then
      echo "::error::expected exactly one .github/rigel-version line at ${commit}, found ${#versions[@]}" >&2
      return 1
    fi
    return 0
  fi
  printf '%s\n' "${versions[0]}"
}

compare_decimal() {
  local left="$1" right="$2"
  [ "${#left}" -gt "${#right}" ] || { [ "${#left}" -eq "${#right}" ] && [[ "$left" > "$right" ]]; }
}

increment_decimal() {
  local number="$1" i digit carry=1 result=
  for ((i = ${#number} - 1; i >= 0; i--)); do
    digit="${number:i:1}"
    if [ "$carry" -eq 1 ]; then
      if [ "$digit" = 9 ]; then
        result="0${result}"
      else
        result="$((digit + 1))${result}"
        carry=0
      fi
    else
      result="${digit}${result}"
    fi
  done
  if [ "$carry" -eq 1 ]; then result="1${result}"; fi
  printf '%s\n' "$result"
}

validate_next_version() {
  local version="$1" base suffix tag max= expected
  if [[ ! "$version" =~ $VERSION_RE ]]; then
    echo "::error::version '${version}' does not match the required X.Y.Z-rigel.N shape" >&2
    return 1
  fi
  base="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
  suffix="${BASH_REMATCH[4]}"
  while IFS= read -r tag; do
    [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-rigel\.([1-9][0-9]*)$ ]] || continue
    [ "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}" = "$base" ] || continue
    if [ -z "$max" ] || compare_decimal "${BASH_REMATCH[4]}" "$max"; then
      max="${BASH_REMATCH[4]}"
    fi
  done < <(git tag --list "v${base}-rigel.*")
  if [ -z "$max" ]; then
    expected="${base}-rigel.1"
  else
    expected="${base}-rigel.$(increment_decimal "$max")"
  fi
  if [ "$version" != "$expected" ]; then
    echo "::error::expected ${expected}, got ${version}" >&2
    return 1
  fi
}

check_pr() {
  local base_sha="$1" head_sha="$2" base_version head_version
  if git diff --quiet "$base_sha" "$head_sha" -- .github/rigel-version; then
    echo '.github/rigel-version is unchanged; no release version check is needed'
    return 0
  fi
  base_version="$(version_at "$base_sha" no)"
  head_version="$(version_at "$head_sha")"
  if [ "$base_version" = "$head_version" ]; then
    echo '.github/rigel-version version is unchanged; no release check is needed'
    return 0
  fi
  validate_next_version "$head_version"
  echo "${head_version} is the next release version"
}

mint() {
  local sha="$1" repo="$2" version tag target parent parent_version response rc actual
  version="$(version_at "$sha")"
  tag="v${version}"
  if git show-ref --verify --quiet "refs/tags/${tag}" || gh api "repos/${repo}/git/ref/tags/${tag}" >/dev/null 2>&1; then
    echo "${tag} already exists; nothing to mint"
    return 0
  fi

  target="$sha"
  while parent="$(git rev-parse --verify "${target}^1" 2>/dev/null)"; do
    parent_version="$(version_at "$parent" no)"
    [ "$parent_version" = "$version" ] || break
    target="$parent"
  done
  validate_next_version "$version"

  set +e
  response="$(gh api --method POST "repos/${repo}/git/refs" -f "ref=refs/tags/${tag}" -f "sha=${target}" 2>&1)"
  rc=$?
  set -e
  if [ "$rc" -eq 0 ]; then
    echo "Created ${tag} at ${target}"
    return 0
  fi
  if [[ "$response" == *'HTTP 422'* && "$response" == *'Reference already exists'* ]]; then
    actual="$(gh api "repos/${repo}/git/ref/tags/${tag}" --jq '.object.sha')"
    if [ "$actual" = "$target" ]; then
      echo "${tag} already points at ${target}"
      return 0
    fi
    echo "::error::${tag} already exists at ${actual}, expected ${target}" >&2
    return 1
  fi
  printf '%s\n' "$response" >&2
  return "$rc"
}

[ "$#" -ge 1 ] || usage
command="$1"
shift
case "$command" in
  check-pr)
    [ "$#" -eq 2 ] || usage
    check_pr "$1" "$2"
    ;;
  mint)
    [ "$#" -eq 2 ] || usage
    mint "$1" "$2"
    ;;
  *) usage ;;
esac
