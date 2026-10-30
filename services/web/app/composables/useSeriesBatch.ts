import type { SeriesPanel } from '~/utils/types'

// Many /stats/series panels in one request (GET /stats/series/batch): each
// request is authenticated afresh, so a page of charts asking one by one
// cost the database a lookup per chart. Each panel keeps its own status, as
// if it had been asked for alone. The platform statistics use the admin
// panels (base "admin/stats/series/batch"), optionally for one organisation.
// provider limits panels split by model provider (the model queue) to one
// configured provider; the API refuses any other name.
export interface BatchedPanel { data: SeriesPanel | null; status: number; error: string | null }
export interface SeriesBatchOptions { base?: string; org?: MaybeRefOrGetter<string | undefined>; provider?: MaybeRefOrGetter<string | undefined>; immediate?: boolean }

export function useSeriesBatch(panels: MaybeRefOrGetter<string[]>, range: MaybeRefOrGetter<string>, repo?: MaybeRefOrGetter<string | undefined>, opts: SeriesBatchOptions = {}) {
  const path = () => {
    const r = repo ? toValue(repo) : undefined
    const o = opts.org ? toValue(opts.org) : undefined
    const p = opts.provider ? toValue(opts.provider) : undefined
    return `${opts.base ?? 'stats/series/batch'}?panels=${encodeURIComponent(toValue(panels).join(','))}&range=${toValue(range)}${r ? `&repo=${encodeURIComponent(r)}` : ''}${o ? `&org=${encodeURIComponent(o)}` : ''}${p ? `&provider=${encodeURIComponent(p)}` : ''}`
  }
  const { data, error, pending, refresh, execute } = useApiGet<{ panels: Record<string, { status: number; body: unknown }> }>(path, { immediate: opts.immediate ?? true })
  const byPanel = computed<Record<string, BatchedPanel>>(() => {
    const out: Record<string, BatchedPanel> = {}
    for (const [name, item] of Object.entries(data.value?.panels ?? {})) {
      const ok = item.status === 200
      out[name] = { data: ok ? item.body as SeriesPanel : null, status: item.status, error: ok ? null : ((item.body as { detail?: string } | null)?.detail ?? `HTTP ${item.status}`) }
    }
    return out
  })
  return { panels: byPanel, error, pending, refresh, execute }
}
