import type { NuxtApp } from '#app'

// All data flows through the Nuxt server proxy at /api/cg, never directly to
// the Go API, so the API token stays on the server.
export function apiPath(path: string): string {
  return `/api/cg/${path.replace(/^\/+/, '')}`
}

// Keys the layout keeps alive and refreshes on its own (the repository list
// polls every 15 s): a page asking for one again reuses the layout's answer
// instead of fetching it a second time. Refreshes still go to the API.
const SHARED_KEYS = new Set(['repos'])
function reuseLive(key: string, nuxtApp: NuxtApp, ctx: { cause: string }) {
  if (nuxtApp.isHydrating) return nuxtApp.payload.data[key]
  if (ctx.cause !== 'initial') return undefined
  const held = nuxtApp._asyncData[key]
  return held?._init && held.status.value === 'success' ? held.data.value : undefined
}

// A smaller view of an answer, for a page that shows a few numbers out of a
// large report: only the view is kept and serialised into the page payload.
// The name keeps its cache key apart from the full answer's.
export interface ApiView<T, R> { name: string; pick: (full: T) => R }

type ApiGet<R> = ReturnType<typeof useFetch<R>>

// The key follows the path: a key read once at setup kept a page on its
// first answer when its window or filter changed.
export function useApiGet<T, R = T>(path: MaybeRefOrGetter<string>, opts: { immediate?: boolean; view?: ApiView<T, R> } = {}) {
  const nuxtApp = useNuxtApp()
  const view = opts.view
  const key = () => (view ? `${toValue(path)}#${view.name}` : toValue(path))
  const k = key()
  // When another component already holds this key (the layout and a page,
  // or one row component per repository), wait for its request instead of
  // cancelling it and asking again. The holder's own refreshes still cancel.
  const held = !!nuxtApp._asyncData[k]?._init
  return useFetch(() => apiPath(toValue(path)), {
    key,
    immediate: opts.immediate ?? true,
    dedupe: held ? 'defer' : 'cancel',
    ...(SHARED_KEYS.has(k) ? { getCachedData: reuseLive } : {}),
    ...(view ? { transform: (full: unknown) => view.pick(full as T) } : {}),
  }) as unknown as ApiGet<R>
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
