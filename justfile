# codesynapse development tasks. `just` lists them.

set dotenv-load := true

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-s -w -X main.version=" + version

# Container runtime: Docker if present, else Podman. Override with COMPOSE="...".
compose := env("COMPOSE", `if command -v docker >/dev/null 2>&1; then echo "docker compose"; elif command -v podman >/dev/null 2>&1; then echo "podman compose"; else echo "missing-container-runtime"; fi`)

default:
    @just --list

# --- stack (docker compose) -------------------------------------------------

# Create .env with random secrets if it does not exist
init:
    #!/usr/bin/env sh
    set -eu
    if [ -f .env ]; then echo ".env exists, leaving it alone"; exit 0; fi
    sed -e "s/change-me-please/$(openssl rand -hex 24)/" \
        -e "s/change-me-api-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-mcp-token/$(openssl rand -hex 24)/" .env.example > .env
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

# Store a token for cloning private repositories (prompted, never echoed), then restart the API.
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
    tmp=$(mktemp)
    grep -v -e '^CODESYNAPSE_GIT_TOKEN=' -e '^CODESYNAPSE_GIT_TOKEN_HOST=' .env > "$tmp" || true
    printf 'CODESYNAPSE_GIT_TOKEN=%s\nCODESYNAPSE_GIT_TOKEN_HOST=%s\n' "$token" "{{host}}" >> "$tmp"
    mv "$tmp" .env && chmod 600 .env
    {{compose}} up -d api
    echo "token stored in .env for {{host}}; the API was restarted"
    echo "read-only token: Contents read. For fix branches and PRs (CODESYNAPSE_ALLOW_PUSH=true): Contents and Pull requests read and write."

# Build and start Neo4j, API, MCP and web
up: init _runtime
    {{compose}} up -d --build
    @echo "web http://localhost:3000 · api http://localhost:8080 · mcp http://localhost:8090/mcp · neo4j http://localhost:7474"

# Start only Neo4j (for running the Go services or web locally)
db: init _runtime
    {{compose}} up -d neo4j

# Stop the stack (data is kept)
down:
    {{compose}} down

# Stop the stack and delete the graph and caches
nuke:
    {{compose}} down -v

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
    go run ./cmd/codesynapse mcp install

# Print an MCP client config for the running stack
mcp-config:
    @echo '{ "mcpServers": { "codesynapse": { "type": "http", "url": "http://localhost:8090/mcp", "headers": { "Authorization": "Bearer '"$CODESYNAPSE_MCP_TOKEN"'" } } } }'

# --- container images and Kubernetes --------------------------------------

registry := env("REGISTRY", "ghcr.io/parisnakitakejser")
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
    {{engine}} build $plat -t codesynapse-web:{{tag}} -t {{registry}}/codesynapse-web:{{tag}} services/web

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
    sed -e "s/change-me-please/$(openssl rand -hex 24)/" \
        -e "s/change-me-api-token/$(openssl rand -hex 24)/" \
        -e "s/change-me-mcp-token/$(openssl rand -hex 24)/" "$dir/secrets.env.example" > "$dir/secrets.env"
    echo "wrote $dir/secrets.env (git-ignored); add CODESYNAPSE_GIT_TOKEN etc. there if you need them"

# Print the manifests an overlay produces
k8s-render overlay="dev":
    kustomize build deploy/k8s/overlays/{{overlay}}

# Apply an overlay to the current kubectl context
k8s-apply overlay="dev":
    kubectl apply -k deploy/k8s/overlays/{{overlay}}
    @echo "kubectl -n codesynapse get pods · kubectl -n codesynapse port-forward svc/web 3000:80"

# --- local development (no containers except Neo4j) ------------------------

# Run the API against the compose Neo4j, serving ./sources
dev-api: db
    CODESYNAPSE_NEO4J_URI=neo4j://localhost:7687 CODESYNAPSE_NEO4J_PASSWORD="$NEO4J_PASSWORD" \
    go run ./services/api/cmd/codesynapse-api serve --sources-root ./sources

# Run the MCP server on stdio against the compose Neo4j
dev-mcp: db
    CODESYNAPSE_NEO4J_URI=neo4j://localhost:7687 CODESYNAPSE_NEO4J_PASSWORD="$NEO4J_PASSWORD" \
    go run ./services/mcp/cmd/codesynapse-mcp serve

# Run the Nuxt dev server against `just dev-api`
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

# Store contract tests against the compose Neo4j (uses repo name "fixture")
test-integration: db
    CODESYNAPSE_TEST_NEO4J_URI=neo4j://localhost:7687 CODESYNAPSE_TEST_NEO4J_PASSWORD="$NEO4J_PASSWORD" \
    go test -race -run TestContract ./internal/store/...

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
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Type-check and build the web app
web-check:
    cd services/web && npm ci --no-fund --no-audit && npm run typecheck && npm run build

# Everything CI runs
ci: lint test web-check
