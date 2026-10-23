#!/usr/bin/env sh
# Only a release publishes (docs/SAAS.md, 5d and 6b): a vX.Y.Z tag or a
# release candidate vX.Y.Z-rcN, on a commit that is on main. Prints the
# version without the v.
set -eu
tag="$1"
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$'; then
	echo "::error::$tag is not a release (vX.Y.Z) or release candidate (vX.Y.Z-rcN) tag" >&2
	exit 1
fi
# A full checkout already has origin/main; fetch only when it is missing.
git rev-parse -q --verify origin/main >/dev/null || git fetch --no-tags origin main >&2
if ! git merge-base --is-ancestor "$(git rev-parse "$tag^{commit}")" origin/main; then
	echo "::error::$tag points at a commit that is not on main" >&2
	exit 1
fi
printf '%s\n' "${tag#v}"
