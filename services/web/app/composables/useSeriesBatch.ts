import type { SeriesPanel } from '~/utils/types'

// Many /stats/series panels in one request (GET /stats/series/batch): each
// request is authenticated afresh, so a page of charts asking one by one
// cost the database a lookup per chart. Each panel keeps its own status, as
// if it had been asked for alone.
export interface BatchedPanel { data: SeriesPanel | null; status: number; error: string | null }

export function useSeriesBatch(panels: MaybeRefOrGetter<string[]>, range: MaybeRefOrGetter<string>, repo?: MaybeRefOrGetter<string | undefined>) {
  const path = () => {
    const r = repo ? toValue(repo) : undefined
    return `stats/series/batch?panels=${encodeURIComponent(toValue(panels).join(','))}&range=${toValue(range)}${r ? `&repo=${encodeURIComponent(r)}` : ''}`
  }
  const { data, error, pending, refresh } = useApiGet<{ panels: Record<string, { status: number; body: unknown }> }>(path)
  const byPanel = computed<Record<string, BatchedPanel>>(() => {
    const out: Record<string, BatchedPanel> = {}
    for (const [name, item] of Object.entries(data.value?.panels ?? {})) {
      const ok = item.status === 200
      out[name] = { data: ok ? item.body as SeriesPanel : null, status: item.status, error: ok ? null : ((item.body as { detail?: string } | null)?.detail ?? `HTTP ${item.status}`) }
    }
    return out
  })
  return { panels: byPanel, error, pending, refresh }
}
