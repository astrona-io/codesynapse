// The graph canvas is pointer-first; these helpers give it a second way in.
// The same ordering drives the node list beside the canvas and the arrow keys
// on the canvas, so both walk the nodes in the same sequence.

interface Named { id: string; name: string; kind: string }
interface Link { from: string; to: string }

// orderNodes lists the nodes with the most connections first, then by name,
// so the first stops of a keyboard walk are the hubs of the graph.
export function orderNodes<N extends Named>(nodes: N[], edges: Link[]): N[] {
  const degree = new Map<string, number>()
  for (const e of edges) {
    degree.set(e.from, (degree.get(e.from) ?? 0) + 1)
    degree.set(e.to, (degree.get(e.to) ?? 0) + 1)
  }
  return [...nodes].sort((a, b) => (degree.get(b.id) ?? 0) - (degree.get(a.id) ?? 0) || a.name.localeCompare(b.name) || a.id.localeCompare(b.id))
}

// stepSelection moves one node along the order (delta 1 or -1), starting at
// the first node when nothing is selected and stopping at the ends.
export function stepSelection(order: { id: string }[], current: string | null | undefined, delta: number): string | null {
  if (order.length === 0) return null
  const at = current ? order.findIndex((n) => n.id === current) : -1
  if (at < 0) return order[delta < 0 ? order.length - 1 : 0]!.id
  return order[Math.min(order.length - 1, Math.max(0, at + delta))]!.id
}

// announce is the sentence a screen reader hears after something changed.
export function announce(o: { busy: boolean; nodes: number; edges: number; selected?: Named | null; connections?: number }): string {
  if (o.busy) return 'Loading the graph'
  if (o.selected) {
    const c = o.connections ?? 0
    return `Selected ${o.selected.name}, ${o.selected.kind}, ${c} ${c === 1 ? 'connection' : 'connections'}`
  }
  return `${o.nodes} ${o.nodes === 1 ? 'node' : 'nodes'} and ${o.edges} ${o.edges === 1 ? 'edge' : 'edges'} loaded`
}
