# codesynapse development tasks. `just` lists them.

set dotenv-load := true

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-s -w -X main.version=" + version

# Container runtime: Docker if present, else Podman. Override with COMPOSE="...".
runtime := `if command -v docker >/dev/null 2>&1; then echo "docker compose"; elif command -v podman >/dev/null 2>&1; then echo "podman compose"; else echo "missing-container-runtime"; fi`
compose := env("COMPOSE", runtime)

default:
    @just --list

# --- stack (docker compose) -------------------------------------------------

# Create .env with random secrets if it does not exist
init:
    #!/usr/bin/env sh
    set -eu
    if [ -f .env ]; then
        # Upgrades: add keys newer versions need, never touch existing ones.
        if ! grep -q '^POSTGRES_PASSWORD=' .env; then
            printf '\n# PostgreSQL for users, organisations, API keys and the audit log.\nPOSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 12)" >> .env
            echo "added POSTGRES_PASSWORD to .env"
        fi
        if ! grep -q '^SECRET_KEY=' .env; then
            printf '\n# Seals organisation Git tokens; keep it (tokens sealed with a lost key must be set again).\nSECRET_KEY=%s\n' "$(openssl rand -base64 32)" >> .env
            echo "added SECRET_KEY to .env"
        fi
        if ! grep -q '^ADMIN_PASSWORD=' .env; then
            printf '\n# Password of the administrator ("admin") made at first start; replace the account after signing in.\nADMIN_PASSWORD=%s\n' "$(openssl rand -hex 16)" >> .env
            echo "added ADMIN_PASSWORD to .env"
        fi
        if ! grep -q '^METRICS_PASSWORD=' .env; then
            printf '\n# VictoriaMetrics password (statistics by minute and hour).\nMETRICS_PASSWORD=%s\n' "$(openssl rand -hex 12)" >> .env
            echo "added METRICS_PASSWORD to .env"
        fi
        if ! grep -q '^NEBULA_PASSWORD=' .env; then
            printf '\n# NebulaGraph root password (the graph database; at most 24 characters).\nNEBULA_PASSWORD=%s\n' "$(openssl rand -hex 12)" >> .env
            echo "added NEBULA_PASSWORD to .env"
        fi
        for svc in API WORKER MCP; do
            if ! grep -q "^NEBULA_${svc}_PASSWORD=" .env; then
                printf '\n# NebulaGraph password of the %s service user (at most 24 characters).\nNEBULA_%s_PASSWORD=%s\n' "$svc" "$svc" "$(openssl rand -hex 12)" >> .env
                echo "added NEBULA_${svc}_PASSWORD to .env"
            fi
        done
        exit 0
    fi
    sed -e "s/change-me-api-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-mcp-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-postgres/$(openssl rand -hex 24)/" \
        -e "s/change-me-nebula-api/$(openssl rand -hex 12)/" \
        -e "s/change-me-nebula-worker/$(openssl rand -hex 12)/" \
        -e "s/change-me-nebula-mcp/$(openssl rand -hex 12)/" \
        -e "s/change-me-nebula/$(openssl rand -hex 12)/" \
        -e "s/change-me-metrics/$(openssl rand -hex 24)/" \
        -e "s/change-me-admin/$(openssl rand -hex 16)/" \
        -e "s|change-me-secret-key|$(openssl rand -base64 32)|" .env.example > .env
    mkdir -p sources
    [ -f sources/go.mod ] || printf "module sources\n" > sources/go.mod
    echo "wrote .env; put repositories (or symlink-free clones) under ./sources"

# Fail early with install hints when no container runtime is found
_runtime:
    #!/usr/bin/env sh
    if [ "{{compose}}" = "missing-container-runtime" ]; then
        echo "No container runtime found. Install one of:" >&2
        echo "  brew install --cask docker        # Docker Desktop" >&2
        echo "  brew install podman docker-compose && podman machine init && podman machine start" >&2
        exit 1
    fi

# Store a token for one git host (prompted, never echoed), then restart the API. Run it once per host.
# Use a fine-grained GitHub token with read-only "Contents" access to the repositories you index.
git-token host="github.com": init _runtime
    #!/usr/bin/env sh
    set -eu
    printf "Token for %s (input hidden): " "{{host}}"
    stty -echo 2>/dev/null || true
    read -r token
    stty echo 2>/dev/null || true
    echo
    [ -n "$token" ] || { echo "no token given, nothing changed"; exit 1; }
    # One entry per host in CODESYNAPSE_GIT_TOKENS (host=token,host2=token2); a token
    # is only ever sent to its own host. Re-running for a host replaces its entry.
    host=$(printf '%s' "{{host}}" | tr 'A-Z' 'a-z')
    tmp=$(mktemp)
    current=$(sed -n 's/^CODESYNAPSE_GIT_TOKENS=//p' .env | head -n 1)
    kept=$(printf '%s' "$current" | tr ',' '\n' | grep -v -e "^$host=" -e '^$' | paste -sd, - || true)
    entries="${kept:+$kept,}$host=$token"
    grep -v -e '^CODESYNAPSE_GIT_TOKENS=' .env > "$tmp" || true
    printf 'CODESYNAPSE_GIT_TOKENS=%s\n' "$entries" >> "$tmp"
    mv "$tmp" .env && chmod 600 .env
    {{compose}} up -d api
    echo "token stored in .env for $host; the API was restarted. Run again with another host (just git-token codeberg.org) to add more."
    echo "read-only token: Contents read. For fix branches and PRs (CODESYNAPSE_ALLOW_PUSH=true): Contents and Pull requests read and write."

# Build and start NebulaGraph, Milvus, PostgreSQL, API, MCP and web
up: init _runtime
    {{compose}} up -d --build
    @echo "web http://localhost:3000 · api http://localhost:8080 · mcp http://localhost:8090/mcp"
    @echo "first sign-in: user admin, password: grep ADMIN_PASSWORD .env (replace the account afterwards)"

# Stop the stack (data is kept)
down:
    {{compose}} down

# Stop the stack and delete the graph and caches
nuke:
    {{compose}} down -v

# Destroy everything (containers, volumes incl. graphs, accounts and audit log, local images, orphans) and recreate from scratch. Keeps .env.
[confirm("This deletes all volumes (indexed graphs, accounts, audit log). Continue?")]
reset: init _runtime
    {{compose}} down -v --remove-orphans --rmi local
    {{compose}} build --no-cache
    {{compose}} up -d --force-recreate
    @echo "web http://localhost:3000 · api http://localhost:8080 · mcp http://localhost:8090/mcp"

# Reset the admin (root) account's password to ADMIN_PASSWORD from .env; "generate" prints a new random one instead
admin-reset mode="": _runtime
    {{compose}} exec api codesynapse-api reset-admin {{ if mode == "generate" { "--generate" } else { "" } }}

# Follow logs, optionally for one service: just logs api
logs service="":
    {{compose}} logs -f {{service}}

# Index a local checkout through the worker (it has the Go toolchain; the API image has none). Named after the checkout's origin remote the way the dashboard names a repository added by URL, so adding that URL there links this index
ingest dir name="":
    #!/usr/bin/env sh
    set -eu
    src="$(cd "{{dir}}" && pwd)"
    base="$(basename "$src")"
    mkdir -p sources
    if [ "$src" != "$(pwd)/sources/$base" ]; then
        rsync -a --delete --exclude /sources --exclude /bin --exclude node_modules --exclude .nuxt --exclude .output "$src/" "sources/$base/"
    fi
    origin="$(git -C "$src" remote get-url origin 2>/dev/null || true)"
    if [ -n "{{name}}" ]; then
        naming="--repo {{name}}"
    elif [ -n "$origin" ]; then
        naming="--name-from-origin"
    else
        naming="--repo $base"
        echo "note: $src has no origin remote, so it is indexed as '$base' and cannot be linked to an organisation by URL" >&2
    fi
    # shellcheck disable=SC2086
    {{compose}} exec worker codesynapse ingest "/sources/$base" $naming --vulns="${CODESYNAPSE_VULN_SCAN:-true}" --allow-network="${CODESYNAPSE_ALLOW_NETWORK:-false}"
    if [ "${CODESYNAPSE_ALLOW_NETWORK:-false}" != true ]; then
        echo "note: module downloads are off, so Go packages whose dependencies are not in the worker's module cache are not type-checked (reported as dependencies_unavailable); set CODESYNAPSE_ALLOW_NETWORK=true for a checkout you trust" >&2
    fi
    if [ -n "$origin" ]; then
        echo "Indexed. To see it in your organisation: sign in as an owner, open Repositories, choose Add and paste $origin (public repositories need no token; a private one uses your organisation's Git token). The dashboard links this index instead of cloning."
    fi

# Index this repository itself (dogfooding)
ingest-self:
    just ingest .

# Print a review context pack for the current branch against a base ref
review base="origin/main":
    go run ./cmd/codesynapse review . --offline --base {{base}}

# Validate the Claude Code plugin and marketplace with Claude Code's own validator (needs the claude CLI)
# Not --strict: the plugin has no version on purpose (installs follow the
# repository's commits) and no icon (no listing in the plugin directory).
plugin-check:
    claude plugin validate ./plugins/codesynapse
    claude plugin validate .

# Score minimal_context and search on past commits against the committed baseline (needs full history)
eval *args:
    go run ./cmd/codesynapse eval . --repo codesynapse --baseline docs/benchmarks/task-eval/codesynapse.json {{args}}

# Register the MCP server with your AI agents (asks per agent: Claude Code, Codex, Gemini, OpenCode, Cursor, project)
mcp:
    go install ./cmd/codesynapse
    @# macOS scans a new binary on its first start, which can outlast Claude Code's 30 s connect timeout.
    "$(go env GOPATH)/bin/codesynapse" --help >/dev/null
    "$(go env GOPATH)/bin/codesynapse" mcp install

# --- container images and Kubernetes --------------------------------------

registry := env("REGISTRY", "ghcr.io/astrona-io")
platform := env("PLATFORM", "")
engine := env("CONTAINER_ENGINE", `if command -v docker >/dev/null 2>&1; then echo docker; elif command -v podman >/dev/null 2>&1; then echo podman; else echo missing-container-runtime; fi`)

# The store images the integration contracts run against: the ones docker-compose.yml ships.
# .github/scripts/check-test-images.sh (part of `just checks`) keeps recipes, Compose and the chart in step.
pg_image := `sh .github/scripts/compose-image.sh codesynapse-postgres`
milvus_image := `sh .github/scripts/compose-image.sh milvus`
nebula_metad_image := `sh .github/scripts/compose-image.sh codesynapse-nebula-metad`
nebula_storaged_image := `sh .github/scripts/compose-image.sh codesynapse-nebula-storaged`
nebula_graphd_image := `sh .github/scripts/compose-image.sh codesynapse-nebula-graphd`
nebula_console_image := `sh .github/scripts/compose-image.sh codesynapse-nebula-console`
victoriametrics_image := `sh .github/scripts/compose-image.sh victoria-metrics`

# Build the api, worker, mcp and web images, tagged codesynapse-<svc>:<tag> and <registry>/codesynapse-<svc>:<tag>. PLATFORM=linux/amd64 for a cluster of another architecture.
image-build tag="dev":
    #!/usr/bin/env sh
    set -eu
    p="{{platform}}"; plat=""; [ -z "$p" ] || plat="--platform $p"
    # api and worker are two targets of one Dockerfile (docs/SAAS.md, 5e).
    for svc in api worker; do
        {{engine}} build $plat --build-arg VERSION={{version}} --target $svc -f services/api/Dockerfile \
            -t codesynapse-$svc:{{tag}} -t {{registry}}/codesynapse-$svc:{{tag}} .
    done
    {{engine}} build $plat --build-arg VERSION={{version}} -f services/mcp/Dockerfile \
        -t codesynapse-mcp:{{tag}} -t {{registry}}/codesynapse-mcp:{{tag}} .
    {{engine}} build $plat --build-arg VERSION={{version}} -f services/web/Dockerfile -t codesynapse-web:{{tag}} -t {{registry}}/codesynapse-web:{{tag}} .

# Scan a built image for fixable HIGH/CRITICAL vulnerabilities (.trivyignore.yaml); types limits it to os packages when the libraries are upstream's. Works the same with Docker and Podman: the image is saved and scanned as a file.
scan-image image types="os,library":
    #!/usr/bin/env sh
    set -eu
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    {{engine}} save -o "$tmp/image.tar" "{{image}}"
    {{engine}} run --rm -v "$tmp:/scan:ro" -v "$PWD/.trivyignore.yaml:/src/.trivyignore.yaml:ro" -w /src -v codesynapse-trivy-cache:/root/.cache ghcr.io/aquasecurity/trivy:0.76.1 \
        image --quiet --exit-code 1 --ignorefile .trivyignore.yaml --scanners vuln --pkg-types {{types}} --severity HIGH,CRITICAL --ignore-unfixed --input /scan/image.tar

# Report-only scan of the store images the chart deploys, as Artifact Hub sees them. scanner: trivy (default) or grype.
scan-chart-images scanner="trivy":
    CONTAINER_ENGINE={{engine}} .github/scripts/scan-chart-images.sh {{scanner}}

# Push the images to REGISTRY (log in first: docker login ghcr.io)
image-push tag="dev":
    #!/usr/bin/env sh
    set -eu
    for svc in api worker mcp web; do {{engine}} push {{registry}}/codesynapse-$svc:{{tag}}; done

# --- Helm chart and a throwaway kind cluster (docs/SAAS.md, 6a) ---------------
# Every kubectl and helm call names the kind context explicitly: these
# recipes never touch another cluster.

kind_cluster := env("KIND_CLUSTER", "codesynapse-dev")
kind_context := "kind-" + kind_cluster
kind_ns := "codesynapse"

# Lint and scan the chart: helm lint, kubeconform, kube-linter, kubesec
chart-check:
    sh .github/scripts/check-chart.sh

# Create the throwaway kind cluster (Podman when Docker is absent)
kind-up:
    #!/usr/bin/env sh
    set -eu
    [ "{{engine}}" = podman ] && export KIND_EXPERIMENTAL_PROVIDER=podman
    if kind get clusters 2>/dev/null | grep -qx "{{kind_cluster}}"; then echo "kind cluster {{kind_cluster}} exists"; exit 0; fi
    kind create cluster --name "{{kind_cluster}}" --wait 120s

# Build the four images and load them into the kind cluster
chart-images tag="e2e":
    #!/usr/bin/env sh
    set -eu
    [ "{{engine}}" = podman ] && export KIND_EXPERIMENTAL_PROVIDER=podman
    just image-build {{tag}}
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    for svc in api worker mcp web; do
        {{engine}} save -o "$tmp/$svc.tar" {{registry}}/codesynapse-$svc:{{tag}}
        kind load image-archive "$tmp/$svc.tar" --name "{{kind_cluster}}"
    done

# The Secret every install needs, with random values (created once)
_kind-secret:
    #!/usr/bin/env sh
    set -eu
    k="kubectl --context {{kind_context}} --namespace {{kind_ns}}"
    kubectl --context "{{kind_context}}" get namespace "{{kind_ns}}" >/dev/null 2>&1 || kubectl --context "{{kind_context}}" create namespace "{{kind_ns}}"
    $k get secret codesynapse >/dev/null 2>&1 || $k create secret generic codesynapse
    # Add any key the Secret lacks, so a cluster set up by an older chart
    # (before NebulaGraph, say) gets the new ones and keeps the rest.
    for kv in CODESYNAPSE_API_TOKEN:hex24 CODESYNAPSE_MCP_TOKEN:hex24 POSTGRES_PASSWORD:hex24 NEBULA_PASSWORD:hex12 NEBULA_API_PASSWORD:hex12 NEBULA_WORKER_PASSWORD:hex12 NEBULA_MCP_PASSWORD:hex12 METRICS_PASSWORD:hex24 SECRET_KEY:b64; do
        key=${kv%%:*}
        if [ -z "$($k get secret codesynapse -o "jsonpath={.data.$key}")" ]; then
            case ${kv#*:} in hex24) v=$(openssl rand -hex 24) ;; hex12) v=$(openssl rand -hex 12) ;; b64) v=$(openssl rand -base64 32) ;; esac
            $k patch secret codesynapse --type merge -p "{\"stringData\":{\"$key\":\"$v\"}}" >/dev/null
        fi
    done

# Install the chart from the working tree into the kind cluster (random secrets, created once)
chart-install: _kind-secret
    helm upgrade --install codesynapse deploy/helm/codesynapse --kube-context "{{kind_context}}" --namespace "{{kind_ns}}" \
        -f deploy/helm/codesynapse/ci/kind-values.yaml --wait --timeout 15m

# Install a published chart version and its images into the kind cluster (release verification). pull_secret names a docker-registry Secret for private packages.
chart-install-published version pull_secret="": _kind-secret
    #!/usr/bin/env sh
    set -eu
    pull=""; [ -z "{{pull_secret}}" ] || pull="--set global.imagePullSecrets[0].name={{pull_secret}}"
    helm upgrade --install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version "{{version}}" \
        --kube-context "{{kind_context}}" --namespace "{{kind_ns}}" -f deploy/helm/codesynapse/ci/kind-values.yaml \
        --set image.tag="{{version}}" --set image.pullPolicy=IfNotPresent $pull --wait --timeout 15m

# Run the chart's tests (every service answers) in the kind cluster
chart-test:
    helm test codesynapse --kube-context "{{kind_context}}" --namespace "{{kind_ns}}" --logs

# Point the kind install at a three-instance CloudNativePG cluster and kill its primary (after chart-install)
chart-failover-postgres:
    sh .github/scripts/postgres-failover.sh "{{kind_context}}" "{{kind_ns}}"

# Point the kind install at a Milvus cluster (SeaweedFS for S3, no MinIO or Pulsar) and kill nodes under load (after chart-install)
chart-failover-milvus:
    sh .github/scripts/milvus-failover.sh "{{kind_context}}" "{{kind_ns}}"

# Install KEDA and check that it scales the workers on queued jobs and back (after chart-install)
chart-scale-keda:
    sh .github/scripts/keda-scale.sh "{{kind_context}}" "{{kind_ns}}"

# Build, load, install and test on a throwaway kind cluster (just kind-down removes it)
chart-e2e: chart-check kind-up chart-images chart-install chart-test
    @echo "the chart works on {{kind_context}}; reach the dashboard with:"
    @echo "  kubectl --context {{kind_context}} --namespace {{kind_ns}} port-forward svc/codesynapse-web 3000:3000"

# Delete the throwaway kind cluster
kind-down:
    #!/usr/bin/env sh
    [ "{{engine}}" = podman ] && export KIND_EXPERIMENTAL_PROVIDER=podman
    kind delete cluster --name "{{kind_cluster}}"

# --- local development --------------------------------------------------------
# There is no database-free MCP server: every tool call needs accounts and a
# registered runtime session. Develop against `just up` and register an agent
# with `just mcp` (or `codesynapse mcp install`); `go run ./services/mcp/cmd/codesynapse-mcp
# serve` against the stack's accounts database is the same server unpacked.

# Run the Nuxt dev server against the API of `just up`
dev-web:
    cd services/web && npm install --no-fund --no-audit && NUXT_API_BASE=http://127.0.0.1:8080 npm run dev

# List the security rules (rules/*/*.yaml), most severe first
rules:
    #!/usr/bin/env sh
    for f in rules/*/*.yaml; do
        awk -v f="$f" '/^id:/{id=$2} /^severity:/{sev=$2} /^title:/{sub(/^title: /,""); t=$0} END{rank["critical"]=0;rank["high"]=1;rank["medium"]=2;rank["low"]=3;rank["info"]=4; printf "%d\t%-8s\t%-26s\t%s\n", rank[sev], sev, id, t}' "$f"
    done | sort | cut -f2-

# --- quality ----------------------------------------------------------------

# Build all Go binaries into ./bin
build:
    mkdir -p bin
    go build -trimpath -ldflags "{{ldflags}}" -o bin/ ./cmd/... ./services/api/cmd/... ./services/mcp/cmd/...

# Unit tests with the race detector
test:
    go test -race ./...

# Accounts contract against a throwaway PostgreSQL container (never the compose database: the test drops its tables)
test-postgres: _runtime
    #!/usr/bin/env sh
    set -eu
    pw=$(openssl rand -hex 16)
    name="codesynapse-pg-test-$$"
    {{engine}} run --rm -d --name "$name" -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=cs {{pg_image}} >/dev/null
    trap '{{engine}} stop "$name" >/dev/null 2>&1' EXIT
    # A container that never comes up fails the run instead of hanging it.
    n=0; until {{engine}} exec "$name" pg_isready -U postgres >/dev/null 2>&1; do n=$((n+1)); [ "$n" -lt 90 ] || { echo "PostgreSQL did not start" >&2; exit 1; }; sleep 1; done
    sleep 1
    CODESYNAPSE_TEST_DATABASE_URL="postgres://postgres:$pw@127.0.0.1:55432/cs?sslmode=disable" go test -race -count=1 ./internal/accounts/...

# Store contract against a throwaway NebulaGraph cluster (the images docker-compose.yml ships) and Milvus
test-nebula: _runtime
    #!/usr/bin/env sh
    set -eu
    id="$$"; net="codesynapse-neb-test-$id"; mv="codesynapse-milvus-test-$id"
    flags="--log_dir=/logs --v=0 --minloglevel=1 --heartbeat_interval_secs=1"
    {{engine}} network create "$net" >/dev/null
    {{engine}} run --rm -d --network "$net" --name "$net-metad" {{nebula_metad_image}} --meta_server_addrs="$net-metad:9559" --local_ip="$net-metad" --ws_ip="$net-metad" --port=9559 --data_path=/data/meta $flags >/dev/null
    {{engine}} run --rm -d --network "$net" --name "$net-storaged" {{nebula_storaged_image}} --meta_server_addrs="$net-metad:9559" --local_ip="$net-storaged" --ws_ip="$net-storaged" --port=9779 --data_path=/data/storage $flags >/dev/null
    {{engine}} run --rm -d --network "$net" --name "$net-graphd" -p 127.0.0.1:59669:9669 {{nebula_graphd_image}} --meta_server_addrs="$net-metad:9559" --local_ip="$net-graphd" --ws_ip="$net-graphd" --port=9669 --enable_authorize=true $flags >/dev/null
    {{engine}} run --rm -d --name "$mv" -p 127.0.0.1:59530:19530 -e ETCD_USE_EMBED=true -e ETCD_DATA_DIR=/var/lib/milvus/etcd \
        -e COMMON_STORAGETYPE=local -e DEPLOY_MODE=STANDALONE {{milvus_image}} milvus run standalone >/dev/null
    trap '{{engine}} stop "$net-metad" "$net-storaged" "$net-graphd" "$mv" >/dev/null 2>&1; {{engine}} network rm "$net" >/dev/null 2>&1' EXIT
    console() { {{engine}} run --rm --network "$net" --entrypoint nebula-console {{nebula_console_image}} -addr "$net-graphd" -port 9669 -u root -p nebula -e "$1"; }
    n=0; until console 'SHOW HOSTS' >/dev/null 2>&1; do n=$((n+1)); [ "$n" -lt 60 ] || { echo "NebulaGraph did not start" >&2; exit 1; }; sleep 2; done
    console "ADD HOSTS \"$net-storaged\":9779" >/dev/null
    n=0; until console 'SHOW HOSTS' | grep -q ONLINE; do n=$((n+1)); [ "$n" -lt 60 ] || { echo "the storage host did not come online" >&2; exit 1; }; sleep 2; done
    n=0; until {{engine}} exec "$mv" curl -sf http://127.0.0.1:9091/healthz >/dev/null 2>&1; do n=$((n+1)); [ "$n" -lt 90 ] || { echo "Milvus did not start" >&2; exit 1; }; sleep 2; done
    export CODESYNAPSE_TEST_NEBULA_ADDR=127.0.0.1:59669 CODESYNAPSE_TEST_MILVUS_URL=http://127.0.0.1:59530
    go test -race -count=1 ./internal/store/nebulastore/... ./internal/vector/...

# Push and tenant-filtered queries against a throwaway VictoriaMetrics
test-metrics: _runtime
    #!/usr/bin/env sh
    set -eu
    name="codesynapse-vm-test-$$"; pw=$(openssl rand -hex 16)
    {{engine}} run --rm -d --name "$name" -p 127.0.0.1:58428:8428 {{victoriametrics_image}} \
        -search.latencyOffset=1s -httpAuth.username=cs -httpAuth.password="$pw" >/dev/null
    trap '{{engine}} stop "$name" >/dev/null 2>&1' EXIT
    n=0; until curl -sf http://127.0.0.1:58428/health >/dev/null; do n=$((n+1)); [ "$n" -lt 60 ] || { echo "VictoriaMetrics did not start" >&2; exit 1; }; sleep 1; done
    CODESYNAPSE_TEST_METRICS_URL=http://127.0.0.1:58428 CODESYNAPSE_TEST_METRICS_USER=cs CODESYNAPSE_TEST_METRICS_PASSWORD="$pw" \
        go test -race -count=1 -run TestVictoriaMetrics ./internal/telemetry/ ./services/api/internal/api/

# Store, accounts and metrics contracts against throwaway NebulaGraph, Milvus, PostgreSQL and VictoriaMetrics containers
test-integration: test-nebula test-postgres test-metrics

# Tests and the shipped stack must use the same store images (see pg_image)
check-test-images:
    sh .github/scripts/check-test-images.sh

# Coverage report
cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1

# Short fuzz runs for the parsers
fuzz time="20s":
    go test ./internal/gitx -run=^$ -fuzz=FuzzParseDiff -fuzztime={{time}}
    go test ./internal/redact -run=^$ -fuzz=FuzzText -fuzztime={{time}}

# Lint Go code
lint:
    golangci-lint run ./...

# Vulnerability scan of Go dependencies
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Type-check and build the web app
web-check:
    cd services/web && npm ci --no-fund --no-audit && npm run typecheck && npm test && npm run build

# --- project checks (CI runs the same recipes) ------------------------------
# Tools run at pinned versions through `go run`, ghcr.io images or uvx, so
# a local run matches CI. Needs Go, uv and a container engine.

# Lint GitHub workflows
lint-actions:
    go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

# Lint the service Dockerfiles (.hadolint.yaml)
lint-docker:
    {{engine}} run --rm -v "$PWD:/src:ro" -w /src ghcr.io/hadolint/hadolint:v2.15.1 hadolint services/api/Dockerfile services/mcp/Dockerfile services/web/Dockerfile deploy/postgres/Dockerfile deploy/nebula/Dockerfile

# Lint shell scripts
lint-shell:
    uvx --quiet --from shellcheck-py==0.11.0.1 shellcheck $(git ls-files '*.sh')

# Lint YAML (.yamllint.yml)
lint-yaml:
    uvx --quiet --from yamllint==1.38.0 yamllint --strict $(git ls-files '*.yml' '*.yaml')

# Alert rules in deploy/alerts: vmalert -dryRun, the rule unit tests and the label guard
lint-alerts:
    CONTAINER_ENGINE={{engine}} sh .github/scripts/check-alerts.sh

# Lint prose in the agent-facing docs (.vale.ini)
lint-prose:
    go run github.com/vale-cli/vale/v3/cmd/vale@v3.25.1 AGENTS.md CLAUDE.md docs/AGENT_GUIDE.md

# Scan the git history for secrets (.gitleaks.toml, .gitleaksignore)
secrets:
    go run github.com/zricethezav/gitleaks/v8@v8.30.1 git . --no-banner --redact

# Vulnerabilities with a fix and misconfigurations, HIGH and CRITICAL (.trivyignore.yaml)
scan:
    {{engine}} run --rm -v "$PWD:/src:ro" -w /src -v codesynapse-trivy-cache:/root/.cache ghcr.io/aquasecurity/trivy:0.76.1 \
        fs --quiet --exit-code 1 --ignorefile .trivyignore.yaml --scanners vuln,misconfig --severity HIGH,CRITICAL --ignore-unfixed \
        --skip-dirs '**/node_modules' --skip-dirs '**/testdata' --skip-dirs .claude --skip-dirs sources --skip-dirs bin .

# Licences of what we ship are permissive (run after web-check)
licenses-check:
    sh .github/scripts/check-licenses.sh

# Every project check besides tests, Go lint and the web build
checks: lint-actions lint-docker lint-shell lint-yaml lint-alerts lint-prose check-test-images secrets scan vuln

# Everything CI runs on a pull request (the integration contracts run separately)
ci: lint test web-check licenses-check checks eval

# --- docs (mkdocs) ------------------------------------------------------------

# Serve the docs with live reload on http://localhost:8000 (needs uv)
docs:
    uvx --with-requirements docs/requirements.txt mkdocs serve -a localhost:8000

# Build the docs into site/, failing on broken links and nav
docs-build:
    uvx --with-requirements docs/requirements.txt mkdocs build --strict
