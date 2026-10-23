#!/bin/sh
# PostgreSQL failover on the throwaway kind cluster (docs/SAAS.md, 6d).
# Runs a three-instance CloudNativePG cluster, points the installed chart at
# it instead of its own PostgreSQL, kills the primary while the API keeps
# writing (every login attempt is a row in login_attempts), and checks that
# writes come back on the new primary and `helm test` still passes.
#
# Usage: postgres-failover.sh <kube-context> <namespace>. Needs the release
# `codesynapse` installed by `just chart-install` in that namespace.
set -eu

ctx=$1
ns=$2
case "$ctx" in kind-*) ;; *) echo "refusing $ctx: only throwaway kind clusters" >&2; exit 1 ;; esac

cnpg_version=1.30.1
cnpg_sha256=37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb
db=codesynapse-db
k="kubectl --context $ctx"

tmp=$(mktemp -d)
pf=""
cleanup() {
	[ -z "$pf" ] || kill "$pf" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT

echo "== CloudNativePG $cnpg_version"
curl -fsSL -o "$tmp/cnpg.yaml" "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$cnpg_version/cnpg-$cnpg_version.yaml"
echo "$cnpg_sha256  $tmp/cnpg.yaml" | shasum -a 256 -c - >/dev/null
$k apply --server-side -f "$tmp/cnpg.yaml" >/dev/null
$k -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout 5m

echo "== three PostgreSQL instances"
$k -n "$ns" apply -f - <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: $db
spec:
  instances: 3
  storage: {size: 1Gi}
  bootstrap:
    initdb: {database: codesynapse, owner: codesynapse}
EOF
$k -n "$ns" wait "cluster/$db" --for=condition=Ready --timeout 10m

echo "== the chart on the external database"
url=$($k -n "$ns" get secret "$db-app" -o jsonpath='{.data.uri}' | base64 -d)
encoded=$(printf '%s' "$url" | base64 | tr -d '\n')
$k -n "$ns" patch secret codesynapse --type merge -p "{\"data\":{\"CODESYNAPSE_DATABASE_URL\":\"$encoded\"}}" >/dev/null
helm upgrade codesynapse deploy/helm/codesynapse --kube-context "$ctx" --namespace "$ns" \
	-f deploy/helm/codesynapse/ci/kind-values.yaml --set postgresql.enabled=false --wait --timeout 15m
helm test codesynapse --kube-context "$ctx" --namespace "$ns" >/dev/null

# A free local port: kubectl picks it and prints it.
$k -n "$ns" port-forward svc/codesynapse-api :8080 >"$tmp/pf.log" 2>&1 &
pf=$!
port=""
i=0
while [ -z "$port" ]; do
	i=$((i + 1)); [ "$i" -lt 30 ] || { cat "$tmp/pf.log" >&2; exit 1; }
	sleep 1
	port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$tmp/pf.log" | head -1)
done
n=0
# 401 (wrong password) and 429 (limited) both mean the attempt reached
# PostgreSQL; anything else means it did not.
probe() {
	n=$((n + 1))
	code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
		--data "{\"email\":\"failover-$n@example.com\",\"password\":\"not-the-password\"}" \
		"http://127.0.0.1:$port/api/v1/auth/login" || true)
	[ "$code" = 401 ] || [ "$code" = 429 ]
}
primary() { $k -n "$ns" get "cluster/$db" -o jsonpath='{.status.currentPrimary}'; }

i=0
until probe; do
	i=$((i + 1)); [ "$i" -lt 30 ] || { echo "the API does not answer" >&2; exit 1; }
	sleep 2
done

old=$(primary)
# Killed at once, as a crashed node would: a graceful delete lets the old
# primary serve its open connections for minutes before handing over.
echo "== killing the primary $old"
$k -n "$ns" delete pod "$old" --grace-period=0 --force --wait=false >/dev/null 2>&1
start=$(date +%s)
failed=0
while :; do
	elapsed=$(($(date +%s) - start))
	[ "$elapsed" -lt 300 ] || { echo "writes did not recover within 300 s" >&2; exit 1; }
	now=$(primary)
	if probe; then
		[ "$now" != "$old" ] && [ -n "$now" ] && break
	else
		failed=$((failed + 1))
	fi
	sleep 2
done
echo "new primary $now after ${elapsed}s; $failed refused writes while it switched"

$k -n "$ns" wait "cluster/$db" --for=condition=Ready --timeout 10m
helm test codesynapse --kube-context "$ctx" --namespace "$ns" --logs
echo "PostgreSQL failover: writes recovered and every service answers"
