#!/usr/bin/env sh
# The integration recipes must test the releases users install: they take their
# images from docker-compose.yml (compose-image.sh), never from a literal, and the
# Helm chart's store images must match what Compose ships. Fails on drift.
set -eu
cd "$(dirname "$0")/../.."
fail=0

# 1. No store image literal in the justfile.
if grep -nE '(postgres|milvus|victoria-metrics|hugegraph/hugegraph):[v0-9]' justfile; then
    echo "justfile: a store image is spelled out; use compose-image.sh so tests follow docker-compose.yml" >&2
    fail=1
fi

# 2. Chart store images equal Compose's.
check() { # name chart-values-file
    want=$(sh .github/scripts/compose-image.sh "$1")
    got=$(sed -n 's/^image:[[:space:]]*\(.*\)$/\1/p' "$2" | head -n 1)
    if [ "$want" != "$got" ]; then
        echo "$2: image is '$got', docker-compose.yml ships '$want'" >&2
        fail=1
    fi
}
check codesynapse-postgres deploy/helm/codesynapse/charts/postgresql/values.yaml
check codesynapse-hugegraph deploy/helm/codesynapse/charts/hugegraph/values.yaml
check milvus deploy/helm/codesynapse/charts/milvus/values.yaml
check victoria-metrics deploy/helm/codesynapse/charts/victoriametrics/values.yaml
check victoria-traces deploy/helm/codesynapse/charts/victoriatraces/values.yaml

[ "$fail" -eq 0 ] && echo "integration images: justfile, Compose and chart agree"
exit "$fail"
