#!/bin/sh
# Milvus cluster failover on the throwaway kind cluster (docs/SAAS.md, 6d-2).
# Runs Milvus in cluster mode from the official chart, without MinIO (AGPLv3)
# or Pulsar: SeaweedFS (Apache-2.0) is the S3 store, the write-ahead log is
# the streaming nodes' embedded Woodpecker on that store, and authentication
# is on. Links the installed chart to it, then kills a query node, a
# streaming node and a coordinator while TestClusterFailover writes and searches through the
# CodeSynapse client: operations must recover, and every confirmed write
# must still be there. `helm test` must pass before and after.
#
# Usage: milvus-failover.sh <kube-context> <codesynapse-namespace>. Needs
# the release `codesynapse` installed by `just chart-install`.
set -eu

ctx=$1
ns=$2
case "$ctx" in kind-*) ;; *) echo "refusing $ctx: only throwaway kind clusters" >&2; exit 1 ;; esac

chart_version=5.0.30
chart_sha256=2e3be7a517d4d9d00be2851a22b7a4c86bdcbac496612cb069c2a31da2832d7f
seaweedfs=docker.io/chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
milvus_tag=v3.0.4
mns=milvus-cluster
k="kubectl --context $ctx"

tmp=$(mktemp -d)
pf=""
cleanup() {
	[ -z "$pf" ] || kill "$pf" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT

$k get namespace "$mns" >/dev/null 2>&1 || $k create namespace "$mns"
# Test credentials, kept in the cluster across runs.
if ! $k -n "$mns" get secret s3 >/dev/null 2>&1; then
	$k -n "$mns" create secret generic s3 --from-literal=access="$(openssl rand -hex 12)" --from-literal=secret="$(openssl rand -hex 24)"
fi
access=$($k -n "$mns" get secret s3 -o jsonpath='{.data.access}' | base64 -d)
secret=$($k -n "$mns" get secret s3 -o jsonpath='{.data.secret}' | base64 -d)

echo "== SeaweedFS as the S3 store"
$k -n "$mns" create secret generic s3-config --dry-run=client -o yaml \
	--from-literal=s3.json="{\"identities\":[{\"name\":\"milvus\",\"credentials\":[{\"accessKey\":\"$access\",\"secretKey\":\"$secret\"}],\"actions\":[\"Admin\",\"Read\",\"Write\",\"List\",\"Tagging\"]}]}" |
	$k apply -f - >/dev/null
$k -n "$mns" apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: seaweedfs}
spec:
  replicas: 1
  selector: {matchLabels: {app: seaweedfs}}
  template:
    metadata: {labels: {app: seaweedfs}}
    spec:
      containers:
        - name: seaweedfs
          image: $seaweedfs
          args: [server, -dir=/data, -s3, -s3.config=/etc/s3/s3.json, -master.volumeSizeLimitMB=256, -volume.max=0]
          ports: [{containerPort: 8333, name: s3}]
          readinessProbe: {tcpSocket: {port: s3}, periodSeconds: 5}
          volumeMounts:
            - {name: data, mountPath: /data}
            - {name: config, mountPath: /etc/s3, readOnly: true}
      volumes:
        - {name: data, emptyDir: {}}
        - {name: config, secret: {secretName: s3-config}}
---
apiVersion: v1
kind: Service
metadata: {name: seaweedfs}
spec:
  selector: {app: seaweedfs}
  ports: [{name: s3, port: 8333, targetPort: s3}]
EOF
$k -n "$mns" rollout status deployment/seaweedfs --timeout 5m
i=0
until $k -n "$mns" exec deploy/seaweedfs -- sh -c 'echo "s3.bucket.list" | weed shell 2>/dev/null' | grep -q milvus; do
	$k -n "$mns" exec deploy/seaweedfs -- sh -c 'echo "s3.bucket.create -name milvus" | weed shell' >/dev/null 2>&1 || true
	i=$((i + 1)); [ "$i" -lt 30 ] || { echo "could not create the bucket" >&2; exit 1; }
	sleep 2
done

echo "== Milvus $milvus_tag in cluster mode (chart $chart_version)"
helm pull milvus --repo https://zilliztech.github.io/milvus-helm --version "$chart_version" --destination "$tmp"
echo "$chart_sha256  $tmp/milvus-$chart_version.tgz" | shasum -a 256 -c - >/dev/null
cat >"$tmp/values.yaml" <<EOF
cluster: {enabled: true}
image: {all: {tag: $milvus_tag}}
minio: {enabled: false}
pulsarv3: {enabled: false}
pulsar: {enabled: false}
kafka: {enabled: false}
woodpecker: {enabled: false}
streaming:
  enabled: true
  woodpecker: {embedded: true, storage: {type: minio}}
externalS3:
  enabled: true
  host: seaweedfs.$mns
  port: "8333"
  accessKey: "$access"
  secretKey: "$secret"
  bucketName: milvus
  useSSL: false
etcd: {replicaCount: 3}
mixCoordinator: {replicas: 2, activeStandby: {enabled: true}}
proxy: {replicas: 2}
queryNode: {replicas: 2}
streamingNode: {replicas: 2}
dataNode: {replicas: 1}
# The chart picks the log only for Pulsar, Kafka or a separate Woodpecker;
# otherwise cluster mode falls back to Pulsar. user.yaml overrides it.
extraConfigFiles:
  user.yaml: |
    mq:
      type: woodpecker
    common:
      security:
        authorizationEnabled: true
EOF
helm upgrade --install vectors "$tmp/milvus-$chart_version.tgz" --kube-context "$ctx" --namespace "$mns" \
	-f "$tmp/values.yaml" --wait --timeout 20m

echo "== the chart on the Milvus cluster"
# Milvus' default first password; a throwaway cluster only.
token=$(printf '%s' "root:Milvus" | base64 | tr -d '\n')
$k -n "$ns" patch secret codesynapse --type merge -p "{\"data\":{\"MILVUS_TOKEN\":\"$token\"}}" >/dev/null
helm upgrade codesynapse deploy/helm/codesynapse --kube-context "$ctx" --namespace "$ns" --reuse-values \
	--set milvus.enabled=false --set milvus.externalURL="http://vectors-milvus.$mns:19530" --wait --timeout 15m
helm test codesynapse --kube-context "$ctx" --namespace "$ns" >/dev/null

$k -n "$mns" port-forward svc/vectors-milvus :19530 >"$tmp/pf.log" 2>&1 &
pf=$!
port=""
i=0
while [ -z "$port" ]; do
	i=$((i + 1)); [ "$i" -lt 30 ] || { cat "$tmp/pf.log" >&2; exit 1; }
	sleep 1
	port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$tmp/pf.log" | head -1)
done

echo "== killing a query node, a streaming node and a coordinator under load"
first() { $k -n "$mns" get pods -l "component=$1" -o jsonpath='{.items[0].metadata.name}'; }
kill_cmd="$k -n $mns delete pod $(first querynode) $(first streamingnode) $(first mixcoord) --grace-period=0 --force --wait=false"
CODESYNAPSE_TEST_MILVUS_URL="http://127.0.0.1:$port" CODESYNAPSE_TEST_MILVUS_TOKEN="root:Milvus" \
	CODESYNAPSE_TEST_MILVUS_FAILOVER="$kill_cmd" \
	go test ./internal/vector/milvus/ -run TestClusterFailover -count=1 -v -timeout 15m

helm test codesynapse --kube-context "$ctx" --namespace "$ns" --logs
echo "Milvus failover: operations recovered, no confirmed write lost, every service answers"
