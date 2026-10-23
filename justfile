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
            printf '\n# PostgreSQL for users, organisations, API keys and the audit log.\nPOSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> .env
            echo "added POSTGRES_PASSWORD to .env"
        fi
        if ! grep -q '^SECRET_KEY=' .env; then
            printf '\n# Seals organisation Git tokens; keep it (tokens sealed with a lost key must be set again).\nSECRET_KEY=%s\n' "$(openssl rand -base64 32)" >> .env
            echo "added SECRET_KEY to .env"
        fi
        if ! grep -q '^METRICS_PASSWORD=' .env; then
            printf '\n# VictoriaMetrics password (statistics by minute and hour).\nMETRICS_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> .env
            echo "added METRICS_PASSWORD to .env"
        fi
        if ! grep -q '^HUGEGRAPH_PASSWORD=' .env; then
            printf '\n# HugeGraph admin password (the graph database).\nHUGEGRAPH_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> .env
            echo "added HUGEGRAPH_PASSWORD to .env"
        fi
        exit 0
    fi
    sed -e "s/change-me-api-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-mcp-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-postgres/$(openssl rand -hex 24)/" \
        -e "s/change-me-hugegraph/$(openssl rand -hex 24)/" \
        -e "s/change-me-metrics/$(openssl rand -hex 24)/" \
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

# Build and start HugeGraph, Milvus, PostgreSQL, API, MCP and web
up: init _runtime
    {{compose}} up -d --build
    @echo "web http://localhost:3000 · api http://localhost:8080 · mcp http://localhost:8090/mcp"

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

# Follow logs, optionally for one service: just logs api
logs service="":
    {{compose}} logs -f {{service}}

# Copy a local checkout into ./sources and ingest it through the API container
ingest dir name="":
    #!/usr/bin/env sh
    set -eu
    src="$(cd "{{dir}}" && pwd)"
    name="{{name}}"; [ -n "$name" ] || name="$(basename "$src")"
    mkdir -p sources
    if [ "$src" != "$(pwd)/sources/$name" ]; then
        rsync -a --delete --exclude /sources --exclude /bin --exclude node_modules --exclude .nuxt --exclude .output "$src/" "sources/$name/"
    fi
    {{compose}} exec api codesynapse ingest "/sources/$name" --repo "$name" --vulns="${CODESYNAPSE_VULN_SCAN:-true}"

# Ingest this repository itself (dogfooding)
ingest-self:
    just ingest . codesynapse

# Print a review context pack for the current branch against a base ref
review base="origin/main":
    go run ./cmd/codesynapse review . --offline --base {{base}}

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

# Build the api, mcp and web images, tagged codesynapse-<svc>:<tag> and <registry>/codesynapse-<svc>:<tag>. PLATFORM=linux/amd64 for a cluster of another architecture.
image-build tag="dev":
    #!/usr/bin/env sh
    set -eu
    p="{{platform}}"; plat=""; [ -z "$p" ] || plat="--platform $p"
    for svc in api mcp; do
        {{engine}} build $plat --build-arg VERSION={{version}} -f services/$svc/Dockerfile \
            -t codesynapse-$svc:{{tag}} -t {{registry}}/codesynapse-$svc:{{tag}} .
    done
    {{engine}} build $plat -f services/web/Dockerfile -t codesynapse-web:{{tag}} -t {{registry}}/codesynapse-web:{{tag}} .

# Scan a built image for fixable HIGH/CRITICAL vulnerabilities (.trivyignore). Works the same with Docker and Podman: the image is saved and scanned as a file.
scan-image image:
    #!/usr/bin/env sh
    set -eu
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    {{engine}} save -o "$tmp/image.tar" "{{image}}"
    {{engine}} run --rm -v "$tmp:/scan:ro" -v "$PWD/.trivyignore:/src/.trivyignore:ro" -w /src -v codesynapse-trivy-cache:/root/.cache ghcr.io/aquasecurity/trivy:0.62.1 \
        image --quiet --exit-code 1 --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --input /scan/image.tar

# Push the images to REGISTRY (log in first: docker login ghcr.io)
image-push tag="dev":
    #!/usr/bin/env sh
    set -eu
    for svc in api mcp web; do {{engine}} push {{registry}}/codesynapse-$svc:{{tag}}; done

# Load the dev images into a local kind cluster
image-load-kind tag="dev" cluster="kind":
    #!/usr/bin/env sh
    set -eu
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    for svc in api mcp web; do
        {{engine}} save -o "$tmp/$svc.tar" {{registry}}/codesynapse-$svc:{{tag}}
        kind load image-archive "$tmp/$svc.tar" --name {{cluster}}
    done

# Create deploy/k8s/overlays/<overlay>/secrets.env with random secrets if it does not exist
k8s-secrets overlay="dev":
    #!/usr/bin/env sh
    set -eu
    dir=deploy/k8s/overlays/{{overlay}}
    if [ -f "$dir/secrets.env" ]; then echo "$dir/secrets.env exists, leaving it alone"; exit 0; fi
    sed -e "s/change-me-api-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-mcp-token/$(openssl rand -hex 24)/" "$dir/secrets.env.example" > "$dir/secrets.env"
    echo "wrote $dir/secrets.env (git-ignored); add CODESYNAPSE_GIT_TOKEN etc. there if you need them"

# Print the manifests an overlay produces
k8s-render overlay="dev":
    kustomize build deploy/k8s/overlays/{{overlay}}

# Apply an overlay to the current kubectl context
k8s-apply overlay="dev":
    kubectl apply -k deploy/k8s/overlays/{{overlay}}
    @echo "kubectl -n codesynapse get pods · kubectl -n codesynapse port-forward svc/web 3000:80"

# --- local development --------------------------------------------------------
# HugeGraph runs Gremlin and is never published to the host, so the API runs
# in compose (`just up`); these recipes work alongside it or without it.

# Run the MCP server on stdio with an in-memory graph of a directory (no database)
dev-mcp dir=".":
    go run ./services/mcp/cmd/codesynapse-mcp serve --offline {{dir}}

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
    {{engine}} run --rm -d --name "$name" -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=cs docker.io/library/postgres:17-alpine >/dev/null
    trap '{{engine}} stop "$name" >/dev/null 2>&1' EXIT
    until {{engine}} exec "$name" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
    sleep 1
    CODESYNAPSE_TEST_DATABASE_URL="postgres://postgres:$pw@127.0.0.1:55432/cs?sslmode=disable" go test -race -count=1 ./internal/accounts/...

# Store contract and parity checks against throwaway HugeGraph and Milvus containers
test-hugegraph: _runtime
    #!/usr/bin/env sh
    set -eu
    hg="codesynapse-hg-test-$$"; mv="codesynapse-milvus-test-$$"
    # Same images and transaction capacity as docker-compose.yml, so a version bump there is tested here.
    {{engine}} run --rm -d --name "$hg" -p 127.0.0.1:58080:8080 --entrypoint sh docker.io/hugegraph/hugegraph:1.7.0 \
        -c "printf '\nvertex.tx_capacity=1000000\nedge.tx_capacity=1000000\n' >> conf/graphs/hugegraph.properties && exec ./docker-entrypoint.sh" >/dev/null
    {{engine}} run --rm -d --name "$mv" -p 127.0.0.1:59530:19530 -e ETCD_USE_EMBED=true -e ETCD_DATA_DIR=/var/lib/milvus/etcd \
        -e COMMON_STORAGETYPE=local -e DEPLOY_MODE=STANDALONE docker.io/milvusdb/milvus:v3.0.3 milvus run standalone >/dev/null
    trap '{{engine}} stop "$hg" "$mv" >/dev/null 2>&1' EXIT
    # REST and Gremlin answer before the graph is bound (1.7 binds DEFAULT-hugegraph): wait for a graph query.
    until curl -sf --compressed -X POST http://127.0.0.1:58080/gremlin -H 'Content-Type: application/json' \
        -d '{"gremlin":"g.V().limit(0).count()","aliases":{"graph":"DEFAULT-hugegraph","g":"__g_DEFAULT-hugegraph"}}' >/dev/null 2>&1; do sleep 2; done
    until {{engine}} exec "$mv" curl -sf http://127.0.0.1:9091/healthz >/dev/null 2>&1; do sleep 2; done
    export CODESYNAPSE_TEST_HUGEGRAPH_URL=http://127.0.0.1:58080 CODESYNAPSE_TEST_MILVUS_URL=http://127.0.0.1:59530 CODESYNAPSE_TEST_HUGEGRAPH_TX_CAPACITY=1000000
    go test -race -count=1 ./internal/store/hugegraphstore/... ./internal/vector/...
    # The parity check indexes this repository; go/packages races under -race.
    go test -count=1 -run TestParityWithReference ./internal/store/hugegraphstore/

# Push and tenant-filtered queries against a throwaway VictoriaMetrics
test-metrics: _runtime
    #!/usr/bin/env sh
    set -eu
    name="codesynapse-vm-test-$$"; pw=$(openssl rand -hex 16)
    {{engine}} run --rm -d --name "$name" -p 127.0.0.1:58428:8428 quay.io/victoriametrics/victoria-metrics:v1.110.0 \
        -search.latencyOffset=1s -httpAuth.username=cs -httpAuth.password="$pw" >/dev/null
    trap '{{engine}} stop "$name" >/dev/null 2>&1' EXIT
    until curl -sf http://127.0.0.1:58428/health >/dev/null; do sleep 1; done
    CODESYNAPSE_TEST_METRICS_URL=http://127.0.0.1:58428 CODESYNAPSE_TEST_METRICS_USER=cs CODESYNAPSE_TEST_METRICS_PASSWORD="$pw" \
        go test -race -count=1 -run TestVictoriaMetrics ./internal/telemetry/ ./services/api/internal/api/

# Store and accounts contracts against throwaway HugeGraph, Milvus and PostgreSQL containers
test-integration: test-hugegraph test-postgres

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
    go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

# Type-check and build the web app
web-check:
    cd services/web && npm ci --no-fund --no-audit && npm run typecheck && npm run build

# --- project checks (CI runs the same recipes) ------------------------------
# Tools run at pinned versions through `go run`, ghcr.io images or uvx, so
# a local run matches CI. Needs Go, uv and a container engine.

# Lint GitHub workflows
lint-actions:
    go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7

# Lint the service Dockerfiles (.hadolint.yaml)
lint-docker:
    {{engine}} run --rm -v "$PWD:/src:ro" -w /src ghcr.io/hadolint/hadolint:v2.12.0 hadolint services/api/Dockerfile services/mcp/Dockerfile services/web/Dockerfile

# Lint shell scripts
lint-shell:
    uvx --quiet --from shellcheck-py==0.11.0.1 shellcheck $(git ls-files '*.sh')

# Lint YAML (.yamllint.yml)
lint-yaml:
    uvx --quiet --from yamllint==1.35.1 yamllint --strict $(git ls-files '*.yml' '*.yaml')

# Scan the git history for secrets (.gitleaks.toml, .gitleaksignore)
secrets:
    go run github.com/zricethezav/gitleaks/v8@v8.24.0 git . --no-banner --redact

# Vulnerabilities with a fix and misconfigurations, HIGH and CRITICAL (.trivyignore)
scan:
    {{engine}} run --rm -v "$PWD:/src:ro" -w /src -v codesynapse-trivy-cache:/root/.cache ghcr.io/aquasecurity/trivy:0.62.1 \
        fs --quiet --exit-code 1 --scanners vuln,misconfig --severity HIGH,CRITICAL --ignore-unfixed \
        --skip-dirs '**/node_modules' --skip-dirs '**/testdata' --skip-dirs .claude --skip-dirs sources --skip-dirs bin .

# Licences of what we ship are permissive (run after web-check)
licenses-check:
    sh .github/scripts/check-licenses.sh

# Every project check besides tests, Go lint and the web build
checks: lint-actions lint-docker lint-shell lint-yaml secrets scan vuln

# Everything CI runs on a pull request (the integration contracts run separately)
ci: lint test web-check licenses-check checks

# --- docs (mkdocs) ------------------------------------------------------------

# Serve the docs with live reload on http://localhost:8000 (needs uv)
docs:
    uvx --with-requirements docs/requirements.txt mkdocs serve -a localhost:8000

# Build the docs into site/, failing on broken links and nav
docs-build:
    uvx --with-requirements docs/requirements.txt mkdocs build --strict
