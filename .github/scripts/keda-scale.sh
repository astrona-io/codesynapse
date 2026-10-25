#!/bin/sh
# Worker scaling with KEDA on the throwaway kind cluster (docs/SAAS.md, 6c).
# Installs KEDA, turns on worker.queueScaling.mode=keda, parks three jobs as
# running in the jobs table (a live lease keeps the workers from claiming
# them) and checks that KEDA scales the worker Deployment up to three, then
# back to one once the jobs are gone.
#
# Usage: keda-scale.sh <kube-context> <namespace>. Needs the release
# `codesynapse` installed by `just chart-install` in that namespace.
set -eu

ctx=$1
ns=$2
case "$ctx" in kind-*) ;; *) echo "refusing $ctx: only throwaway kind clusters" >&2; exit 1 ;; esac

keda_version=2.17.2
keda_sha256=1912c29059e41e80b6abe8b29a566e07337f85fc4bf4fd5698c637fe63ecbf35
k="kubectl --context $ctx"

tmp=$(mktemp -d)
psql() {
	pod=$($k -n "$ns" get pod -l app.kubernetes.io/name=postgresql -o jsonpath='{.items[0].metadata.name}')
	pw=$($k -n "$ns" get secret codesynapse -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)
	$k -n "$ns" exec "$pod" -- env PGPASSWORD="$pw" psql -h 127.0.0.1 -U codesynapse -d codesynapse -v ON_ERROR_STOP=1 -tA -c "$1"
}
cleanup() {
	psql "DELETE FROM jobs WHERE id LIKE 'keda-test-%'" >/dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap cleanup EXIT

# replicas waits until the worker Deployment asks for n replicas.
replicas() {
	want=$1
	limit=$2
	i=0
	while [ "$($k -n "$ns" get deployment codesynapse-worker -o jsonpath='{.spec.replicas}')" != "$want" ]; do
		i=$((i + 1))
		if [ "$i" -gt "$limit" ]; then
			echo "worker did not reach $want replicas" >&2
			$k -n "$ns" get scaledobject,hpa,deployment codesynapse-worker >&2 || true
			$k -n keda logs deployment/keda-operator --tail 30 >&2 || true
			exit 1
		fi
		sleep 5
	done
}

echo "== KEDA $keda_version"
curl -fsSL -o "$tmp/keda.yaml" "https://github.com/kedacore/keda/releases/download/v$keda_version/keda-$keda_version.yaml"
echo "$keda_sha256  $tmp/keda.yaml" | shasum -a 256 -c - >/dev/null
$k apply --server-side -f "$tmp/keda.yaml" >/dev/null
for d in keda-operator keda-metrics-apiserver keda-admission; do
	$k -n keda rollout status "deployment/$d" --timeout 5m
done

echo "== the chart with worker.queueScaling.mode=keda"
helm upgrade codesynapse deploy/helm/codesynapse --kube-context "$ctx" --namespace "$ns" \
	-f deploy/helm/codesynapse/ci/kind-values.yaml --wait --timeout 15m \
	--set worker.queueScaling.mode=keda,worker.queueScaling.minReplicas=1,worker.queueScaling.maxReplicas=3 \
	--set worker.queueScaling.cooldownSeconds=60,global.kedaNamespace=keda
$k -n "$ns" wait scaledobject/codesynapse-worker --for=condition=Ready --timeout 3m
replicas 1 24

echo "== three running jobs: scale up"
psql "INSERT INTO jobs (id, kind, repo, org_id, actor_id, via, role, execution, state, lease_owner, lease_until, created_at)
SELECT 'keda-test-' || n, 'sync', 'keda/test-' || n, 'org', 'user', 'session', 'admin', '{}', 'running', 'keda-test', now() + interval '1 hour', now()
FROM generate_series(1, 3) n" >/dev/null
replicas 3 48

echo "== no jobs: scale down"
psql "DELETE FROM jobs WHERE id LIKE 'keda-test-%'" >/dev/null
replicas 1 120

echo "KEDA scaled the workers up to 3 and back to 1"
