#!/usr/bin/env sh
# Report-only scan of the store images the chart deploys (docs/operations/
# running.md). Artifact Hub scans these too, so this shows the same numbers
# before a release. We cannot fix upstream images, so nothing fails here.
# usage: scan-chart-images.sh [trivy|grype]   (CONTAINER_ENGINE defaults to docker)
set -eu
scanner="${1:-trivy}"
engine="${CONTAINER_ENGINE:-docker}"
here="$(dirname "$0")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for ref in $("$here/chart-images.sh"); do
	case "$scanner" in
	trivy)
		"$engine" run --rm -v codesynapse-trivy-cache:/root/.cache ghcr.io/aquasecurity/trivy:0.76.1 \
			image --quiet --scanners vuln --format json "$ref" >"$tmp/out.json" || echo "scan failed" >&2
		;;
	grype)
		"$engine" run --rm -v codesynapse-grype-cache:/.cache/grype ghcr.io/anchore/grype:v0.121.0 \
			"$ref" -o json >"$tmp/out.json" || echo "scan failed" >&2
		;;
	*)
		echo "unknown scanner $scanner (trivy or grype)" >&2
		exit 2
		;;
	esac
	python3 -I "$here/summarise-scan.py" "$scanner" "$ref" "$tmp/out.json"
done
