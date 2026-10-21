# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f services/api/Dockerfile .
ARG GO_VERSION=1.27

FROM docker.io/library/golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY services/api ./services/api
COPY internal ./internal
COPY rules ./rules
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codesynapse-api ./services/api/cmd/codesynapse-api && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codesynapse ./cmd/codesynapse

# Runtime keeps the Go toolchain: ingest type-checks repositories with `go list`.
FROM docker.io/library/golang:${GO_VERSION}-alpine
RUN apk add --no-cache git ca-certificates && \
    adduser -D -u 10001 codesynapse && \
    mkdir -p /cache/go-build /cache/gomod /workspace && chown -R codesynapse /cache /workspace
ENV GOCACHE=/cache/go-build GOMODCACHE=/cache/gomod GOTOOLCHAIN=local \
    CODESYNAPSE_API_ADDR=0.0.0.0:8080 CODESYNAPSE_SOURCES_ROOT=/sources
COPY --from=build /out/ /usr/local/bin/
USER codesynapse
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["codesynapse-api"]
CMD ["serve"]
