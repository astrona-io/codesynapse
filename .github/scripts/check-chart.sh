#!/usr/bin/env sh
# Lints and scans the Helm chart the way CI does (`just chart-check`):
# helm lint, kubeconform (schemas), kube-linter (best practice) and kubesec
# (security scoring) on the chart rendered with every optional part on.
set -eu

chart=deploy/helm/codesynapse
values=$chart/ci/test-values.yaml
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

helm lint --strict "$chart" -f "$values"
helm template codesynapse "$chart" -f "$values" > "$out/rendered.yaml"
# The kind values must render too.
helm template codesynapse "$chart" -f "$chart/ci/kind-values.yaml" > /dev/null
# So must every store linked from outside (no subchart schema applies then).
helm template codesynapse "$chart" -f "$values" \
    --set postgresql.enabled=false,hugegraph.enabled=false,milvus.enabled=false \
    --set victoriametrics.enabled=false,victoriatraces.enabled=false \
    --set hugegraph.externalURL=https://hugegraph.example.com:8080 > /dev/null

# values.schema.json (and each store's) must refuse what the chart would
# ignore or break on: unknown keys, wrong types, invalid enums.
for bad in api.replica=2 'worker.roles=ingest\,typo' image.pullPolicy=Sometimes \
    auditRetentionDays=7 postgresql.sizee=1Gi hugegraph.size=lots; do
    if helm template codesynapse "$chart" -f "$values" --set "$bad" > /dev/null 2>&1; then
        echo "values.schema.json accepted $bad" >&2
        exit 1
    fi
done

go run github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0 -strict -summary -kubernetes-version 1.33.0 "$out/rendered.yaml"
go run golang.stackrox.io/kube-linter/cmd/kube-linter@v0.8.3 lint "$out/rendered.yaml"

go run github.com/controlplaneio/kubesec/v2@v2.15.0 scan "$out/rendered.yaml" > "$out/kubesec.json" || true
# Secrets as environment variables is accepted: the services read
# credentials from the environment only (AGENTS.md); mounting them as files
# is a follow-up. Anything else critical, or a negative score, fails.
python3 - "$out/kubesec.json" <<'PY'
import json, sys
results = json.load(open(sys.argv[1]))
accepted = {"SecretsAsEnvironmentVariables"}
bad = []
for r in results:
    critical = [c["id"] for c in ((r.get("scoring") or {}).get("critical") or [])]
    if r.get("score", 0) < 0 or set(critical) - accepted:
        bad.append(f'{r.get("object")}: score {r.get("score")}, critical {critical}')
print(f"kubesec: {len(results)} resources checked")
if bad:
    print("kubesec findings:\n  " + "\n  ".join(bad))
    sys.exit(1)
PY
