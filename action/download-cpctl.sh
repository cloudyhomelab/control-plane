#!/usr/bin/env bash
# Installs the release cpctl at $RUNNER_TEMP/cpctl. Only released binaries run: REF must be a
# release tag or the commit SHA one points at. Called by action.yml with REPO, REF, OS, ARCH.
set -euo pipefail

release_tag='^v[0-9]+\.[0-9]+\.[0-9]+$'
commit_sha='^[0-9a-f]{40}$'

fail() {
  echo "::error::$1" >&2
  exit 1
}

# The release asset suffix for this runner.
goarch() {
  if [[ "$OS" != Linux ]]; then
    fail "cpctl is released for Linux only; this runner is $OS/$ARCH."
  fi
  case "$ARCH" in
    X64) echo amd64 ;;
    ARM64) echo arm64 ;;
    *) fail "cpctl is released for x64 and arm64 only; this runner is $OS/$ARCH." ;;
  esac
}

# The release tag REF names, or the one pointing at the commit REF names.
resolve_tag() {
  if [[ -z "$REPO" ]]; then
    fail "Use the action as cloudyhomelab/control-plane/action@<release tag or its commit SHA>, not a local path."
  fi

  if [[ "$REF" =~ $release_tag ]]; then
    echo "$REF"
  elif [[ "$REF" =~ $commit_sha ]]; then
    tag_for_commit "$REF"
  else
    fail "Ref '$REF' is not a release; pin a release tag (vX.Y.Z) or the commit it points at."
  fi
}

tag_for_commit() {
  local sha="$1" refs tag
  refs="$(git ls-remote --tags "$GITHUB_SERVER_URL/$REPO.git")" \
    || fail "Could not list the tags of $REPO."

  # Release tags are annotated, so the commit they point at is on the peeled ^{} line.
  tag="$(sed -nE "s#^${sha}[[:space:]]+refs/tags/(v[0-9]+\.[0-9]+\.[0-9]+)(\^\{\})?\$#\1#p" <<< "$refs" \
    | sort -uV | tail -1)"

  if [[ -z "$tag" ]]; then
    fail "Commit $sha is not a release; pin a release tag or the commit it points at."
  fi
  echo "$tag"
}

main() {
  local cpu tag base dir asset
  cpu="$(goarch)"
  tag="$(resolve_tag)"
  base="$GITHUB_SERVER_URL/$REPO/releases/download/$tag"
  dir="$RUNNER_TEMP/cpctl-release"
  asset="linux-$cpu-cpctl"

  mkdir -p "$dir"
  if ! curl -fsSL --retry 3 -o "$dir/SHA256SUMS" "$base/SHA256SUMS" \
     || ! curl -fsSL --retry 3 -o "$dir/$asset" "$base/$asset"; then
    fail "Could not download cpctl from the $tag release."
  fi

  (cd "$dir" && grep " $asset\$" SHA256SUMS | sha256sum -c -)
  install -m 755 "$dir/$asset" "$RUNNER_TEMP/cpctl"

  if [[ "$("$RUNNER_TEMP/cpctl" version)" != "${tag#v}" ]]; then
    fail "cpctl from $tag does not report ${tag#v}."
  fi
  echo "cpctl ${tag#v} from $base"
}

main
