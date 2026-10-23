#!/usr/bin/env bash
# Checks the Homebrew formula homebrew-formula.sh generates, against a
# locally built codesynapse — what the tap would ship, before a release:
#
#   1. renders the formula for a fake release (vX.Y.Z, SHA256SUMS of the
#      binary built here) and runs `brew style` and `brew audit --strict` on it;
#   2. installs a copy of it pointed at the local binary (file:// URL) and
#      runs `brew test`.
#
# The installed copy is named codesynapse-formula-ci and is keg-only, so it
# never replaces or shadows a codesynapse already installed with Homebrew.
# The temporary tap is removed on exit. Needs: brew, go, a macOS or Linux host.
set -euo pipefail

version="0.0.1"
tag="v${version}"
tap="codesynapse-ci/formula-test"
root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"

export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_ENV_HINTS=1 HOMEBREW_NO_INSTALL_CLEANUP=1

cleanup() {
  brew uninstall --force --quiet "${tap}/codesynapse-formula-ci" >/dev/null 2>&1 || true
  brew untap --force --quiet "$tap" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
esac
asset="codesynapse-${os}-${arch}"

echo "==> Building ${asset} (${tag})"
mkdir -p "$work/dist"
(cd "$root" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${tag}" -o "$work/dist/${asset}" ./cmd/codesynapse)

# The other three platforms only need well-formed checksums here.
for a in codesynapse-darwin-arm64 codesynapse-darwin-amd64 codesynapse-linux-arm64 codesynapse-linux-amd64; do
  if [[ "$a" == "$asset" ]]; then
    (cd "$work/dist" && shasum -a 256 "$a")
  else
    echo "$(printf '0%.0s' {1..64})  $a"
  fi
done > "$work/dist/SHA256SUMS"

"$root/.github/scripts/homebrew-formula.sh" "$tag" "$work/dist/SHA256SUMS" > "$work/codesynapse.rb"

brew tap-new --no-git "$tap" >/dev/null
tapdir="$(brew --repository "$tap")"

echo "==> brew style / brew audit --strict"
cp "$work/codesynapse.rb" "$tapdir/Formula/codesynapse.rb"
brew style "${tap}/codesynapse"
brew audit --strict --formula "${tap}/codesynapse"
rm "$tapdir/Formula/codesynapse.rb"

echo "==> brew install + brew test (local binary)"
url="https://github.com/astrona-io/codesynapse/releases/download/${tag}/${asset}"
# perl, not sed: BSD sed (macOS) doesn't turn \n into a newline.
URL="$url" LOCAL="file://${work}/dist/${asset}" VERSION="$version" perl -pe '
  s/\Q$ENV{URL}\E/$ENV{LOCAL}/;
  s/^class Codesynapse < Formula/class CodesynapseFormulaCi < Formula/;
  s/^  license "Apache-2.0"/  version "$ENV{VERSION}"\n  license "Apache-2.0"\n\n  keg_only "formula test copy, must not shadow a real codesynapse"/;
' "$work/codesynapse.rb" > "$tapdir/Formula/codesynapse-formula-ci.rb"
grep -q "file://${work}/dist/${asset}" "$tapdir/Formula/codesynapse-formula-ci.rb"

brew install "${tap}/codesynapse-formula-ci"
brew test "${tap}/codesynapse-formula-ci"

prefix="$(brew --prefix "${tap}/codesynapse-formula-ci")"
"$prefix/bin/codesynapse" --version | grep -q "$tag"
test -s "$prefix/share/zsh/site-functions/_codesynapse"
test -s "$prefix/share/fish/vendor_completions.d/codesynapse.fish"
echo "==> Formula OK"
