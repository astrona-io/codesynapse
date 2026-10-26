// All data flows through the Nuxt server proxy at /api/cg, never directly to
// the Go API, so the API token stays on the server.
export function apiPath(path: string): string {
  return `/api/cg/${path.replace(/^\/+/, '')}`
}

// The key follows the path: a key read once at setup kept a page on its
// first answer when its window or filter changed.
export function useApiGet<T>(path: MaybeRefOrGetter<string>, opts: { immediate?: boolean } = {}) {
  return useFetch<T>(() => apiPath(toValue(path)), { key: () => toValue(path), immediate: opts.immediate ?? true })
}

// apiGet is a one-off GET for code that fetches on its own schedule (a
// debounced search); pages use useApiGet.
export function apiGet<T>(path: string, opts: { signal?: AbortSignal } = {}): Promise<T> {
  return $fetch(apiPath(path), { signal: opts.signal }) as Promise<T>
}

export function apiPost<T>(path: string, body: unknown): Promise<T> {
  return $fetch(apiPath(path), { method: 'POST', body: body as Record<string, unknown> }) as Promise<T>
}

export function apiPut<T>(path: string, body: unknown): Promise<T> {
  return $fetch(apiPath(path), { method: 'PUT', body: body as Record<string, unknown> }) as Promise<T>
}

export function apiPatch<T>(path: string, body: unknown): Promise<T> {
  return $fetch(apiPath(path), { method: 'PATCH', body: body as Record<string, unknown> }) as Promise<T>
}

export function apiDelete(path: string): Promise<unknown> {
  return $fetch(apiPath(path), { method: 'DELETE' })
}

export function apiErrorMessage(err: unknown): string {
  const e = err as { data?: { detail?: string; statusMessage?: string }; message?: string }
  return e?.data?.detail ?? e?.data?.statusMessage ?? e?.message ?? String(err)
}

export function repoPath(repo: string, rest: string): string {
  return `repos/${encodeURIComponent(repo)}/${rest}`
}
