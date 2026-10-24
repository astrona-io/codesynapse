# Releasing

A release publishes the `codesynapse` CLI (bare binaries on GitHub and a formula in the Homebrew tap [astrona-io/homebrew-tap](https://github.com/astrona-io/homebrew-tap)) and the server stack (container images and the Helm chart, see [Server release](#server-release)).

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
5. Builds the tagged documentation with MkDocs and publishes it to [GitHub Pages](https://astrona-io.github.io/codesynapse/).

A tag with a `-`, such as `v0.3.0-rc1`, is a pre-release. It gets a GitHub release, never becomes "latest", and doesn't touch the tap.

The Release workflow doesn't mark a final release "latest" itself. The Images workflow does that once the published stack passed its install test (below).

## Documentation site

The shareable documentation URL is **https://astrona-io.github.io/codesynapse/**. Each stable release replaces the site with documentation built from that release's tag. Pre-releases do not replace the published documentation. This is a single current-release site, not a version archive.

The Release workflow calls `.github/workflows/docs.yml` directly after creating the release: releases created using `GITHUB_TOKEN` do not trigger a separate release-event workflow. The documentation workflow also handles stable releases published manually, and supports **Actions → Documentation → Run workflow** for the initial publication or a retry. Select the intended release tag when retrying; manually choosing another ref publishes that ref's documentation. Documentation pull requests build with `--strict` but cannot deploy.

For initial repository setup, enable **Settings → Pages → Source → GitHub Actions**. The `github-pages` environment must allow stable release tags and any ref deliberately used for a manual publication. Deployment uses `pages: write` and `id-token: write`; it does not require a long-lived deployment token. Private repositories do not deploy. The deployment job records the published URL in its environment.

Validate documentation locally with `just docs-build`. A failed documentation build leaves the existing site in place; retry the failed workflow after fixing the issue. Documentation publishing is independent of the container/chart installation check that controls the release's latest status.

## Server release

The same tag runs `.github/workflows/images.yml`. It refuses a tag that isn't `vX.Y.Z` or `vX.Y.Z-rcN` on a commit on `main` (`.github/scripts/release-tag.sh`), then:

1. **Images:** builds `codesynapse-{api,worker,mcp,web}` for amd64 and arm64, pushes them as `:X.Y.Z` and `:X.Y` (an RC only as `:X.Y.Z-rcN`), with SBOM and provenance, and signs them with cosign (keyless, GitHub OIDC).
2. **Chart:** packages `deploy/helm/codesynapse` with chart and app version `X.Y.Z`, pushes it to `oci://ghcr.io/astrona-io/charts/codesynapse` and signs it. The images it deploys are written into its `artifacthub.io/images` annotation (an RC is also marked `artifacthub.io/prerelease`), and `deploy/helm/artifacthub-repo.yml` is pushed as the `artifacthub.io` tag. [Artifact Hub](https://artifacthub.io/packages/search?repo=codesynapse) checks the repository about every 30 minutes and lists new versions on its own; the `repositoryID` in that file makes astrona.io its verified publisher. The GHCR package must be public for Artifact Hub to read it.
3. **Verify:** checks the signatures of the chart and the four images, creates a throwaway kind cluster, installs the **published** chart with the **published** images (`just chart-install-published`) and runs `helm test`.
4. **Latest:** for a final release only, moves the images' `:latest` tag to the verified manifests and marks the GitHub release latest.

A failure in steps 1 to 3 leaves the previous release as latest. Fix forward with a new tag; don't move a published tag.

Try a published version on your own kind cluster (`just kind-up` first). For a private package, create a `docker-registry` Secret in the `codesynapse` namespace and pass its name:

```sh
just chart-install-published 0.3.0          # public packages
just chart-install-published 0.3.0 ghcr     # with the pull secret ghcr
just chart-test
```

Upgrading an installation: [Upgrading](upgrading.md).

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
