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
