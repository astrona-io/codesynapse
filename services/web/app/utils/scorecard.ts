// OpenSSF Scorecard of a dependency's source project, as the API sends it.
export interface ScorecardScore { score: number; date: string; checks?: { name: string; score: number }[] }

// Below LOW_SCORECARD is what the security report lists as a hygiene note
// (hygiene.LowScorecard on the server); from GOOD_SCORECARD on it is tinted
// as good.
export const LOW_SCORECARD = 4
export const GOOD_SCORECARD = 7

/** The badge class for a score: pink below 4, amber below 7, green from 7. */
export function scorecardBadge(score: number): string {
  if (score < LOW_SCORECARD) return 'badge badge--pink'
  return score < GOOD_SCORECARD ? 'badge badge--amber' : 'badge badge--green'
}

/** The hover text: the overall score, the day it was scored and the lowest checks. */
export function scorecardTitle(sc: ScorecardScore): string {
  const head = `OpenSSF Scorecard ${sc.score.toFixed(1)} of 10, scored on ${sc.date}`
  const checks = (sc.checks ?? []).map((c) => `${c.name} ${c.score}`)
  return checks.length ? `${head}. Lowest checks: ${checks.join(', ')}` : head
}

/**
 * The Scorecard stored on a graph node ("Name:score" checks), undefined when
 * it has none; a score of 0 is a score, so the date decides.
 */
export function scorecardOfNode(n: { scorecard?: number; scorecard_date?: string; scorecard_checks?: string[] }): ScorecardScore | undefined {
  if (!n.scorecard_date) return undefined
  const checks = (n.scorecard_checks ?? []).flatMap((c) => {
    const i = c.lastIndexOf(':')
    const score = Number(c.slice(i + 1))
    return i > 0 && c.length > i + 1 && Number.isInteger(score) ? [{ name: c.slice(0, i), score }] : []
  })
  return { score: n.scorecard ?? 0, date: n.scorecard_date, checks }
}
