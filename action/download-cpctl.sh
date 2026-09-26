#!/usr/bin/env bash
# Installs the release cpctl at $RUNNER_TEMP/cpctl. Only released binaries run: REF must be a
# release tag or the commit SHA one points at. Called by action.yml with REPO, REF, OS, ARCH.
set -euo pipefail

fail() { echo "::error::$1" >&2; exit 1; }
case "$ARCH" in X64) goarch=amd64 ;; ARM64) goarch=arm64 ;; *) goarch="" ;; esac
[[ "$OS" == Linux && -n "$goarch" ]] \
  || fail "cpctl is released for Linux x64 and arm64 only; this runner is $OS/$ARCH."
[[ -n "$REPO" ]] \
  || fail "Use the action as cloudyhomelab/control-plane/action@<release tag or its commit SHA>, not a local path."
if [[ "$REF" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  tag="$REF"
elif [[ "$REF" =~ ^[0-9a-f]{40}$ ]]; then
  # Release tags are annotated, so the commit they point at is on the peeled ^{} line.
  refs="$(git ls-remote --tags "$GITHUB_SERVER_URL/$REPO.git")" || fail "Could not list the tags of $REPO."
  tag="$(sed -nE "s#^${REF}[[:space:]]+refs/tags/(v[0-9]+\.[0-9]+\.[0-9]+)(\^\{\})?\$#\1#p" <<< "$refs" | sort -uV | tail -1)"
  [[ -n "$tag" ]] || fail "Commit $REF is not a release; pin a release tag or the commit it points at."
else
  fail "Ref '$REF' is not a release; pin a release tag (vX.Y.Z) or the commit it points at."
fi
base="$GITHUB_SERVER_URL/$REPO/releases/download/$tag"
dir="$RUNNER_TEMP/cpctl-release"
mkdir -p "$dir"
if ! curl -fsSL --retry 3 -o "$dir/SHA256SUMS" "$base/SHA256SUMS" \
   || ! curl -fsSL --retry 3 -o "$dir/linux-$goarch-cpctl" "$base/linux-$goarch-cpctl"; then
  fail "Could not download cpctl from the $tag release."
fi
(cd "$dir" && grep " linux-$goarch-cpctl\$" SHA256SUMS | sha256sum -c -)
install -m 755 "$dir/linux-$goarch-cpctl" "$RUNNER_TEMP/cpctl"
[[ "$("$RUNNER_TEMP/cpctl" version)" == "${tag#v}" ]] || fail "cpctl from $tag does not report ${tag#v}."
echo "cpctl ${tag#v} from $base"
