# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f services/mcp/Dockerfile .
ARG GO_VERSION=1.27

FROM docker.io/library/golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY services/mcp ./services/mcp
COPY internal ./internal
COPY rules ./rules
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codesynapse-mcp ./services/mcp/cmd/codesynapse-mcp

# git is needed for impact analysis by base/head refs.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates && adduser -D -u 10001 codesynapse
ENV CODESYNAPSE_MCP_TRANSPORT=http CODESYNAPSE_MCP_ADDR=0.0.0.0:8090
COPY --from=build /out/codesynapse-mcp /usr/local/bin/codesynapse-mcp
USER codesynapse
EXPOSE 8090
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s CMD wget -qO- http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["codesynapse-mcp"]
CMD ["serve"]
