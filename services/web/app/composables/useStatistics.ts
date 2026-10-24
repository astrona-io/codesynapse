import type { WorkspaceStats } from '~/utils/types'

// The Statistics pages share one window (the last 7, 30 or 90 days, kept in
// the URL as ?days=) and one fetch of the workspace figures.
export const STAT_WINDOWS = [7, 30, 90]

export const useStatistics = () => {
  const route = useRoute()
  const router = useRouter()
  const q = Number(route.query.days)
  const days = useState<number>('stats-days', () => (STAT_WINDOWS.includes(q) ? q : 30))
  watch(days, (d) => router.replace({ query: { ...route.query, days: d } }))
  const { data: s, error, pending } = useApiGet<WorkspaceStats>(() => `stats?days=${days.value}`)
  return { days, s, error, pending }
}
