// AI usage by provider from the metrics store. Jobs run in the worker, so the
// API process's own counters (llm.Registry.Usage) miss them; these series
// count every process. ready stays false when live metrics are off.
const PANELS = ['llm_provider_requests', 'llm_provider_failures', 'llm_provider_input', 'llm_provider_output'] as const

export interface ModelUsage { requests: number; failures: number; input: number; output: number }

export function useModelUsage(range: MaybeRefOrGetter<string>) {
  // The four panels in one request (useSeriesBatch).
  const { panels } = useSeriesBatch([...PANELS], range)
  const ready = computed(() => PANELS.every(panel => panels.value[panel]?.data))
  function usageOf(name: string): ModelUsage | null {
    if (!ready.value) return null
    const get = (panel: string) => panels.value[panel]?.data?.series.find(s => s.name === name)?.total ?? 0
    return { requests: get('llm_provider_requests'), failures: get('llm_provider_failures'), input: get('llm_provider_input'), output: get('llm_provider_output') }
  }
  return { ready, usageOf }
}

// The metrics store keeps at most 30 days of series: longer windows show the
// last 30 days.
export const seriesRangeFor = (days: number) => (days <= 7 ? '7d' : '30d')
