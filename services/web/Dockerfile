# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f services/web/Dockerfile .
# The built app is plain JavaScript, so it is built once on the build
# machine for every target platform.
FROM --platform=$BUILDPLATFORM docker.io/library/node:24-alpine AS build
WORKDIR /app
COPY services/web/package.json services/web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY services/web/ ./
RUN npm run build

FROM docker.io/library/node:24-alpine
# The server only runs node: drop the package managers the base image ships
# (and the vulnerabilities in their bundled dependencies).
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
    /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /opt/yarn-* /usr/local/bin/yarn /usr/local/bin/yarnpkg
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT=3000
LABEL org.opencontainers.image.licenses="Apache-2.0" org.opencontainers.image.vendor="Astrona.io"
COPY LICENSE NOTICE /usr/share/licenses/codesynapse/
COPY --from=build --chown=node:node /app/.output ./.output
USER node
EXPOSE 3000
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:3000/api/health || exit 1
CMD ["node", ".output/server/index.mjs"]
