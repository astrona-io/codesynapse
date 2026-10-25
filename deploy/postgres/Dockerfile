# PostgreSQL for the chart and docker-compose: the official image with gosu
# rebuilt on our Go version. Upstream ships gosu built with an older Go, whose
# standard library carries dozens of HIGH/CRITICAL findings in every scan
# (Artifact Hub, Trivy) that only upstream's gosu rebuild can clear. Nothing
# else is changed; bump PG_IMAGE with the base and GOSU_VERSION when it moves.
ARG GO_VERSION=1.27
ARG PG_IMAGE=docker.io/library/postgres:18-alpine

FROM docker.io/library/golang:${GO_VERSION}-alpine AS gosu
# Pinned to a commit: a moved tag must not change what we build.
ARG GOSU_VERSION=1.20
ARG GOSU_COMMIT=29b939aa5bd4dc588af765b8248f6e39150d2565
RUN apk add --no-cache git \
    && git clone --quiet https://github.com/tianon/gosu /src \
    && git -C /src checkout --quiet "$GOSU_COMMIT" \
    && cd /src \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-d -s -w" -o /gosu . \
    && /gosu --version

FROM ${PG_IMAGE}
COPY --from=gosu /gosu /usr/local/bin/gosu
# Alpine packages the base has not caught up with yet (a rebuild picks them up).
RUN apk upgrade --no-cache
