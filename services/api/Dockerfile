# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f services/api/Dockerfile .
ARG GO_VERSION=1.27

# The build stage runs on the build machine and cross-compiles for each
# target platform, so multi-arch builds need no emulation for Go.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY services/api ./services/api
COPY internal ./internal
COPY rules ./rules
ARG VERSION=dev
ARG TARGETOS=linux TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codesynapse-api ./services/api/cmd/codesynapse-api && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codesynapse ./cmd/codesynapse

# Runtime keeps the Go toolchain: ingest type-checks repositories with `go list`.
FROM docker.io/library/golang:${GO_VERSION}-alpine
RUN apk add --no-cache git ca-certificates && \
    adduser -D -u 10001 codesynapse && \
    mkdir -p /cache/go-build /cache/gomod /workspace && chown -R codesynapse /cache /workspace
ENV GOCACHE=/cache/go-build GOMODCACHE=/cache/gomod GOTOOLCHAIN=local \
    CODESYNAPSE_API_ADDR=0.0.0.0:8080 CODESYNAPSE_SOURCES_ROOT=/sources
LABEL org.opencontainers.image.licenses="Apache-2.0" org.opencontainers.image.vendor="Astrona.io"
COPY LICENSE NOTICE /usr/share/licenses/codesynapse/
COPY --from=build /out/ /usr/local/bin/
USER codesynapse
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["codesynapse-api"]
CMD ["serve"]
