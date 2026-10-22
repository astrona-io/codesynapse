# Kubernetes

Kustomize manifests for the whole stack, built from the Dockerfiles in `services/`.

```
deploy/k8s/
  base/            neo4j (StatefulSet), api, mcp, web, config, network policies
  overlays/dev/    local cluster: images from `just image-build dev`, small volume, no ingress
  overlays/prod/   registry images, Ingress for the dashboard and MCP endpoint
```

| Service | Image | Port | State |
|---|---|---|---|
| `neo4j` | `neo4j:5.26-community` | 7687 | 10Gi volume (StatefulSet) |
| `api` | `services/api/Dockerfile` | 8080 | `workspace` volume (clones), Go build cache in `emptyDir` |
| `mcp` | `services/mcp/Dockerfile` | 8090 | reads the same `workspace` volume read-only |
| `web` | `services/web/Dockerfile` | 3000 (service port 80) | none |

## Try it on a local kind cluster

Always test on a throwaway cluster, never on a shared one.

```sh
kind create cluster --name codesynapse
just image-build dev                       # api, mcp and web images
just image-load-kind dev codesynapse       # load them into the cluster
just k8s-secrets dev                       # writes overlays/dev/secrets.env (git-ignored)
just k8s-apply dev
kubectl -n codesynapse port-forward svc/web 3000:80     # http://localhost:3000
```

## Deploy to a cluster

1. Build for the cluster's architecture and push: `PLATFORM=linux/amd64 REGISTRY=ghcr.io/you just image-build v0.1.0 && REGISTRY=ghcr.io/you just image-push v0.1.0`.
2. In `overlays/prod/kustomization.yaml` set the registry and tag in `images:`, and optionally the storage class.
3. In `overlays/prod/ingress.yaml` set your hosts, TLS secret and `ingressClassName`.
4. `just k8s-secrets prod`, then add `CODESYNAPSE_GIT_TOKEN` or `CODESYNAPSE_GIT_TOKENS=host=token,host2=token2` (private repositories, one token per git host) or AI provider keys to `overlays/prod/secrets.env`.
5. `just k8s-render prod` to review, `just k8s-apply prod` to apply.

`secrets.env` is turned into the `codesynapse-secrets` Secret by kustomize and is git-ignored. For production, prefer your own secret tooling (sealed-secrets, external-secrets) and drop the `secretGenerator`; the manifests only expect a Secret named `codesynapse-secrets` with the keys in `secrets.env.example`.

## Notes

- **Workspace volume:** it is `ReadWriteOnce`. `mcp` therefore uses pod affinity to run on the node where `api` runs. With a `ReadWriteMany` storage class, remove the affinity in `base/mcp.yaml`. Keep `api` at one replica (`Recreate` strategy): it owns the workspace and the sync scheduler.
- **Repositories:** a cluster has no `./sources` folder. Add repositories by URL or through **Organisations** in the dashboard. For private repositories set `CODESYNAPSE_GIT_TOKENS`, for example `github.com=ghp_…,codeberg.org=…`.
- **AI providers:** optional. `kubectl -n codesynapse create configmap codesynapse-llm --from-file=llm.yaml=config/llm.yaml`, and put cloud API keys in `secrets.env`.
- **Settings** such as `CODESYNAPSE_ALLOW_PUSH` live in `base/config.yaml`.
- **Pod security:** the namespace enforces `restricted`: non-root, read-only root filesystem, no capabilities, `RuntimeDefault` seccomp.
- **Exposure:** only `web` and `mcp` are meant to be reachable from outside. The API is behind the dashboard, and `neo4j` accepts connections only from `api` and `mcp`. The network policies need a CNI that enforces them.
