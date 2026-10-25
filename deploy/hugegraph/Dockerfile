# HugeGraph for the chart and docker-compose: the official image with the
# Ubuntu packages brought up to date. Upstream ships 22.04 as built, so every
# scan (Artifact Hub, Trivy) reports hundreds of OS findings that a rebuild
# clears. The bundled jars (jackson, netty, hadoop) are upstream's to update;
# nothing here touches them.
ARG HUGEGRAPH_IMAGE=docker.io/hugegraph/hugegraph:1.7.0
FROM ${HUGEGRAPH_IMAGE}
RUN apt-get update \
    && apt-get -y --no-install-recommends upgrade \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
