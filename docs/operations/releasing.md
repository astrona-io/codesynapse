# Releasing

A release publishes the `codesynapse` CLI: bare binaries on GitHub and a formula in the Homebrew tap [astrona-io/homebrew-tap](https://github.com/astrona-io/homebrew-tap). The server stack ships as container images, separately (see [Running the stack](running.md)).

## Cut a release

Tag `main` and push the tag:

```sh
git switch main && git pull
git tag -a v0.3.0 -m "v0.3.0"
git push origin v0.3.0
```

`.github/workflows/release.yml` then:

1. Builds `codesynapse-{darwin,linux}-{amd64,arm64}` with `CGO_ENABLED=0` and stamps the tag into `codesynapse --version`. Each binary gets a signed build provenance attestation.
2. Creates the GitHub release with the four binaries, a `SHA256SUMS` file and generated release notes.
3. Renders `Formula/codesynapse.rb` with `.github/scripts/homebrew-formula.sh` and pushes it to the tap as `codesynapse X.Y.Z`.
4. Installs the formula from the tap on macOS and Ubuntu runners and checks the version matches the tag.

A tag with a `-`, such as `v0.3.0-rc1`, is a pre-release. It gets a GitHub release, never becomes "latest", and doesn't touch the tap.

## Requirements

- **Public repository.** Homebrew can't download private release assets, so steps 3 and 4 are skipped while the repository is private. Tags still produce GitHub releases.
- **`HOMEBREW_TAP_TOKEN` secret** on this repository: a fine-grained PAT with Contents read/write on `astrona-io/homebrew-tap` only. astrona-cli uses the same kind of token for its formula.

## Checking a formula change

`.github/workflows/homebrew.yml` runs on pull requests that touch the formula scripts, the release workflow, `cmd/codesynapse` or `go.mod`. It runs `.github/scripts/test-homebrew-formula.sh` on macOS and Linux. The script renders the formula for a fake `v0.0.1`, runs `brew style` and `brew audit --strict`, then installs a keg-only copy built from the checkout and runs `brew test`. Run it locally the same way; it cleans up after itself.

Don't edit `Formula/codesynapse.rb` in the tap by hand. The next release overwrites it.

## Verifying a download

```sh
gh release download v0.3.0 -R astrona-io/codesynapse -p 'codesynapse-darwin-arm64' -p SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
gh attestation verify codesynapse-darwin-arm64 -R astrona-io/codesynapse
```
