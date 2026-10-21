# Kubernetes manifests

One workload per service, built from the Dockerfiles in `services/`:

| Service | Image build | Port | Notes |
|---|---|---|---|
| `neo4j` | `neo4j:5.26-community` | 7687 bolt, 7474 http | persistent volume on `/data` |
| `api` | `services/api/Dockerfile` | 8080 | needs `/sources` (ro), `/workspace` and `/cache` volumes, `/config/llm.yaml` |
| `mcp` | `services/mcp/Dockerfile` | 8090 | `/sources` and `/workspace` read-only, same paths as `api` |
| `web` | `services/web/Dockerfile` | 3000 | `NUXT_API_BASE` points at the `api` service |

Environment variables and hardening (read-only root fs, no capabilities, tokens from secrets) mirror `docker-compose.yml` at the repository root. Build context for `api` and `mcp` is the repository root; for `web` it is `services/web`.
