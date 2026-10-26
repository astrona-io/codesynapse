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
    --set postgresql.enabled=false,nebula.enabled=false,milvus.enabled=false \
    --set victoriametrics.enabled=false,victoriatraces.enabled=false,victorialogs.enabled=false \
    --set nebula.externalAddrs=nebula.example.com:9669 > /dev/null

# values.schema.json (and each store's) must refuse what the chart would
# ignore or break on: unknown keys, wrong types, invalid enums.
for bad in api.replica=2 'worker.roles=ingest\,typo' 'worker.roles=ingest\,fix' fixWorker.roles=ai image.pullPolicy=Sometimes \
    auditRetentionDays=7 postgresql.sizee=1Gi nebula.size=lots \
    nebula.space.replicas=2 nebula.space.partitions=0 nebula.space.extra=1 \
    worker.restrictEgress=maybe 'worker.egressAllow[0].cidr=nonsense' 'worker.egressAllow[0].extra=1'; do
    if helm template codesynapse "$chart" -f "$values" --set "$bad" > /dev/null 2>&1; then
        echo "values.schema.json accepted $bad" >&2
        exit 1
    fi
done

# Queue scaling renders the object of the tool the cluster has, and only that
# one; the fix worker is never scaled. The CRD of KEDA is not installed here,
# so these are not run through kubeconform.
for mode in hpa keda; do
    helm template codesynapse "$chart" -f "$values" --set "worker.queueScaling.mode=$mode" > "$out/scaling-$mode.yaml"
done
grep -q 'kind: HorizontalPodAutoscaler' "$out/scaling-hpa.yaml" && grep -q 'name: "codesynapse_jobs_active"' "$out/scaling-hpa.yaml"
grep -q 'kind: ScaledObject' "$out/scaling-keda.yaml" && ! grep -q 'name: "codesynapse_jobs_active"' "$out/scaling-keda.yaml"
if grep -q 'kind: ScaledObject' "$out/rendered.yaml"; then echo "KEDA objects rendered with queueScaling off" >&2; exit 1; fi
for bad in worker.queueScaling.mode=argo worker.queueScaling.jobsPerWorker=0 worker.queueScaling.extra=1; do
    if helm template codesynapse "$chart" -f "$values" --set "$bad" > /dev/null 2>&1; then
        echo "values.schema.json accepted $bad" >&2
        exit 1
    fi
done
if helm template codesynapse "$chart" -f "$values" --set worker.queueScaling.mode=hpa,worker.queueScaling.minReplicas=5 > /dev/null 2>&1; then
    echo "chart rendered queueScaling with maxReplicas below minReplicas" >&2
    exit 1
fi

# A replicated graph space needs a NebulaGraph cluster: the in-chart one has
# a single storaged host. Linked to a cluster, the layout reaches every pod.
if helm template codesynapse "$chart" -f "$values" --set nebula.space.replicas=3 > /dev/null 2>&1; then
    echo "chart rendered nebula.space.replicas=3 with the in-chart NebulaGraph" >&2
    exit 1
fi
helm template codesynapse "$chart" -f "$values" --set nebula.enabled=false,nebula.externalAddrs=nebula.example.com:9669,nebula.space.replicas=3,nebula.space.partitions=30 > "$out/nebula-cluster.yaml"
grep -q 'name: CODESYNAPSE_NEBULA_REPLICAS, value: "3"' "$out/nebula-cluster.yaml"
grep -q 'name: CODESYNAPSE_NEBULA_PARTITIONS, value: "30"' "$out/nebula-cluster.yaml"

# The fix worker pushes: it cannot be on without the switch that allows it.
if helm template codesynapse "$chart" -f "$values" --set features.allowPush=false > /dev/null 2>&1; then
    echo "chart rendered a fix worker without features.allowPush" >&2
    exit 1
fi
# No worker holds the API's service token or the first administrator's
# password, and only the fix worker may push.
python3 - "$out/rendered.yaml" <<'PY'
import sys, re
docs = open(sys.argv[1]).read().split("\n---\n")
seen = set()
for d in docs:
    m = re.search(r"^kind: Deployment$", d, re.M)
    name = re.search(r"^  name: (\S+)$", d, re.M)
    if not m or not name or "worker" not in name.group(1):
        continue
    seen.add(name.group(1))
    for secret in ("CODESYNAPSE_API_TOKEN", "CODESYNAPSE_ADMIN_PASSWORD"):
        assert secret not in d, f"{name.group(1)} holds {secret}"
    push = re.search(r"name: CODESYNAPSE_ALLOW_PUSH, value: \"(\w+)\"", d)
    want = "true" if name.group(1).endswith("fix-worker") else "false"
    assert push and push.group(1) == want, f"{name.group(1)}: CODESYNAPSE_ALLOW_PUSH should be {want}"
    assert ("CODESYNAPSE_GIT_PUSH_TOKENS" in d) == (want == "true"), f"{name.group(1)}: push tokens belong to the fix worker only"
assert len(seen) == 2, f"expected a worker and a fix worker, found {sorted(seen)}"
print("workers: no service token or admin password; only the fix worker may push")
PY

# Restricted egress gives both worker kinds an Egress policy that names no
# private range (except through worker.egressAllow).
python3 - "$out/rendered.yaml" <<'PY'
import sys, re
docs = open(sys.argv[1]).read().split("\n---\n")
found = [d for d in docs if re.search(r"^kind: NetworkPolicy$", d, re.M) and "policyTypes: [Ingress, Egress]" in d]
assert len(found) == 2, f"expected egress policies for worker and fix-worker, found {len(found)}"
for d in found:
    assert "10.0.0.0/8" in d and "169.254.0.0/16" in d, "egress policy must exclude private ranges"
print("workers: egress limited to DNS, stores and public HTTPS")
PY

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
