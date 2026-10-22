export function fmtInt(n: number | undefined): string {
  return (n ?? 0).toLocaleString('en-US')
}
export function pct(part: number, whole: number): string {
  return whole > 0 ? `${Math.round((part / whole) * 100)}%` : '–'
}
export function relTime(iso?: string): string {
  if (!iso) return '–'
  const d = Date.parse(iso)
  if (Number.isNaN(d)) return iso
  const s = Math.round((Date.now() - d) / 1000)
  if (s < 60) return 'just now'
  const units: [number, string][] = [[60, 'min'], [3600, 'h'], [86400, 'd'], [604800, 'w'], [2629800, 'mo'], [31557600, 'y']]
  let label = `${Math.round(s / 60)} min`
  for (const [secs, unit] of units) if (s >= secs) label = `${Math.round(s / secs)} ${unit}`
  return `${label} ago`
}
export function location(n: { file?: string; start_line?: number; end_line?: number }): string {
  if (!n.file) return ''
  if (!n.start_line) return n.file
  return n.end_line && n.end_line > n.start_line ? `${n.file}:${n.start_line}-${n.end_line}` : `${n.file}:${n.start_line}`
}
export function shortName(n: { kind?: string; qualified_name?: string; name: string }): string {
  const q = n.qualified_name || n.name
  if (n.kind === 'File' || n.kind === 'Package' || n.kind === 'Document' || n.kind === 'Section') return q
  return q.replace(/(?:[A-Za-z0-9_.~-]+\/)+/g, '')
}
// fmtCompact shortens large numbers for stat tiles and axes: 1,284 / 12.9K / 4.2M.
export function fmtCompact(n: number | undefined): string {
  const v = n ?? 0
  const abs = Math.abs(v)
  if (abs < 10_000) return Math.round(v).toLocaleString('en-US')
  if (abs < 1_000_000) return `${(v / 1000).toFixed(abs < 100_000 ? 1 : 0)}K`
  return `${(v / 1_000_000).toFixed(1)}M`
}

// fmtUnit formats a chart value in its unit: seconds as ms or s, dollars
// with cents, everything else compact.
export function fmtUnit(v: number | null | undefined, unit: string): string {
  if (v == null) return '–'
  if (unit === 'seconds') return v < 1 ? `${Math.round(v * 1000)} ms` : `${v.toFixed(v < 10 ? 2 : 1)} s`
  if (unit === 'USD') return `$${v < 1 ? v.toFixed(3) : v.toFixed(2)}`
  if (Math.abs(v) < 10 && !Number.isInteger(v)) return v.toFixed(1)
  return fmtCompact(v)
}
