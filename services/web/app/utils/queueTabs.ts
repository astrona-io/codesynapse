// Tabs of the model queue page: "" is the overview, every other id a model
// endpoint (provider) the queue answer listed.

// queueTabOf is the tab ?model= names, or the overview for anything the
// answer does not list (an old link, a provider since removed).
export function queueTabOf(query: unknown, names: readonly string[]): string {
  const v = Array.isArray(query) ? query[0] : query
  return typeof v === 'string' && names.includes(v) ? v : ''
}

// queueTabKey moves between tabs with the arrow keys, Home and End, as the
// WAI-ARIA tabs pattern asks; null for any other key.
export function queueTabKey(ids: readonly string[], current: string, key: string): string | null {
  if (!ids.length) return null
  const i = Math.max(ids.indexOf(current), 0)
  switch (key) {
    case 'ArrowRight': return ids[(i + 1) % ids.length]!
    case 'ArrowLeft': return ids[(i - 1 + ids.length) % ids.length]!
    case 'Home': return ids[0]!
    case 'End': return ids[ids.length - 1]!
    default: return null
  }
}
