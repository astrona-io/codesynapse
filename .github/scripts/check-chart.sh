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
