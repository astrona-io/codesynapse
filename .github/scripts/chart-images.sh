#!/usr/bin/env sh
# Prints the third-party images the chart's store subcharts deploy, one per
# line: the top-level `image:` of each charts/<store>/values.yaml, the
# `images:` map of charts/nebula, and alerting.image (vmalert) of the
# parent chart. Our own images are built and
# gated in images.yml.
set -eu
dir="$(dirname "$0")/../../deploy/helm/codesynapse"
{
    sed -n 's/^image: *//p' "$dir"/charts/*/values.yaml
    sed -nE 's/^  (metad|storaged|graphd|console): *//p' "$dir"/charts/nebula/values.yaml
    sed -n 's#^  image: *\(.*/vmalert:.*\)$#\1#p' "$dir"/values.yaml
} | sort -u
