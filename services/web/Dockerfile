# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f services/web/Dockerfile .
FROM docker.io/library/node:24-alpine AS build
WORKDIR /app
COPY services/web/package.json services/web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY services/web/ ./
RUN npm run build

FROM docker.io/library/node:24-alpine
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT=3000
LABEL org.opencontainers.image.licenses="Apache-2.0" org.opencontainers.image.vendor="Astrona.io"
COPY LICENSE NOTICE /usr/share/licenses/codesynapse/
COPY --from=build --chown=node:node /app/.output ./.output
USER node
EXPOSE 3000
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:3000/api/health || exit 1
CMD ["node", ".output/server/index.mjs"]
