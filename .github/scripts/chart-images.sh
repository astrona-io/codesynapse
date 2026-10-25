#!/usr/bin/env sh
# Prints the third-party images the chart's store subcharts deploy, one per
# line: the top-level `image:` of each charts/<store>/values.yaml. Our own
# images are built and gated in images.yml.
set -eu
dir="$(dirname "$0")/../../deploy/helm/codesynapse/charts"
sed -n 's/^image: *//p' "$dir"/*/values.yaml | sort -u
