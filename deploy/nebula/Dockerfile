# syntax=docker/dockerfile:1
# NebulaGraph for the chart and docker-compose: upstream's v3.8.0 binaries on
# a current base. Upstream ships them on CentOS 7, which is end of life: its
# packages (binutils, vim, python 2, curl 7.29, glibc 2.17) add about 1,700
# findings to every image (Artifact Hub, Trivy) that no update will fix, and
# the servers use none of them; they link only against glibc. The binaries
# and their files are copied unchanged, so data, flags and ports stay the
# same. The console is built from its v3.8.0 source with our Go version.
#   docker build --target metad|storaged|graphd|console deploy/nebula
ARG GO_VERSION=1.27

# Upstream images, pinned by digest: a moved tag must not change what we ship.
FROM docker.io/vesoft/nebula-metad:v3.8.0@sha256:ab687bd32d3e441d436842b41427ba0961a5e169c46997ef03fa4dcfd5388b35 AS metad-dist
FROM docker.io/vesoft/nebula-storaged:v3.8.0@sha256:7142642ee69001a5b5c50520196538d58bb2ec3930707c067cb4c4e2be3890f9 AS storaged-dist
FROM docker.io/vesoft/nebula-graphd:v3.8.0@sha256:1040573cc684ea6cc5e673b667422e9abab607c9d553c2c17c7bac6ad8a74e05 AS graphd-dist

# Static Go builds run on the build machine and cross-compile.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}-alpine AS build
ARG TARGETOS=linux TARGETARCH
# nebula-console v3.8.0.
ARG CONSOLE_COMMIT=a103b926bda8ff6ab326df79277b6facb72a8358
# Its go.mod pins golang.org/x modules from 2023; these match ours (Renovate
# keeps them current through the comments).
# renovate: datasource=go depName=golang.org/x/net
ARG X_NET=v0.62.0
# renovate: datasource=go depName=golang.org/x/sys
ARG X_SYS=v0.49.0
# renovate: datasource=go depName=golang.org/x/text
ARG X_TEXT=v0.43.0
RUN apk add --no-cache git \
    && git clone --quiet https://github.com/vesoft-inc/nebula-console /console \
    && git -C /console checkout --quiet "$CONSOLE_COMMIT"
WORKDIR /console
RUN go get golang.org/x/net@"$X_NET" golang.org/x/sys@"$X_SYS" golang.org/x/text@"$X_TEXT" \
    && go mod tidy \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/nebula-console .
WORKDIR /probe
COPY probe/main.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/nebula-probe main.go

# glibc and nothing else: no shell, no package manager. Runs as root like
# upstream, because compose volumes made by the upstream images are root's;
# the chart runs every container as uid 10001.
FROM gcr.io/distroless/base-nossl-debian13:latest@sha256:ca1c7e6a875e59345ae6ff182a1e0930f0ca1ffef23a9b830f8823ab348ad5dd AS server
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/nebula-probe /usr/local/bin/nebula-probe
WORKDIR /usr/local/nebula

FROM server AS metad
COPY --from=metad-dist /usr/local/nebula/ /usr/local/nebula/
EXPOSE 9559 9560 19559 19560
ENTRYPOINT ["/usr/local/nebula/bin/nebula-metad", "--flagfile=/usr/local/nebula/etc/nebula-metad.conf", "--daemonize=false", "--containerized=true"]

FROM server AS storaged
COPY --from=storaged-dist /usr/local/nebula/ /usr/local/nebula/
EXPOSE 9777 9778 9779 9780 19779 19780
ENTRYPOINT ["/usr/local/nebula/bin/nebula-storaged", "--flagfile=/usr/local/nebula/etc/nebula-storaged.conf", "--daemonize=false", "--containerized=true"]

FROM server AS graphd
COPY --from=graphd-dist /usr/local/nebula/ /usr/local/nebula/
EXPOSE 9669 19669 19670
ENTRYPOINT ["/usr/local/nebula/bin/nebula-graphd", "--flagfile=/usr/local/nebula/etc/nebula-graphd.conf", "--daemonize=false", "--containerized=true"]

# The init step's script needs a shell, so the console stays on Alpine.
FROM docker.io/library/alpine:3.24 AS console
RUN apk upgrade --no-cache && adduser -D -u 10001 nebula
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/nebula-console /usr/local/bin/nebula-console
USER 10001
ENTRYPOINT ["nebula-console"]
