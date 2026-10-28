<script setup lang="ts">
// Small dependency-free force-directed layout rendered as SVG. Good for the
// few hundred nodes the API returns; not meant for whole-repo graphs.
import type { ViewEdge, ViewNode } from '~/utils/types'

const props = defineProps<{ nodes: ViewNode[]; edges: ViewEdge[]; root?: string; height?: number }>()
const emit = defineEmits<{ select: [id: string] }>()

const width = 900
const height = computed(() => props.height ?? 560)
interface P { id: string; x: number; y: number; vx: number; vy: number; n: ViewNode }
// Plain objects: the layout is computed once per graph and never mutated
// after, so deep reactivity on every coordinate would only cost proxies.
const points = shallowRef<P[]>([])
const hovered = ref<string | null>(null)
// Two graphs on one page must not share the arrow marker.
const arrow = `${useId()}-arrow`

function layout() {
  const n = props.nodes.length
  const pts: P[] = props.nodes.map((node, i) => {
    const a = (i / Math.max(1, n)) * Math.PI * 2
    const r = node.id === props.root ? 0 : 120 + (node.depth ?? 1) * 60
    return { id: node.id, x: width / 2 + Math.cos(a) * r, y: height.value / 2 + Math.sin(a) * r, vx: 0, vy: 0, n: node }
  })
  const index = new Map(pts.map((p) => [p.id, p]))
  const links = props.edges.map((e) => [index.get(e.from), index.get(e.to)]).filter((l): l is [P, P] => !!l[0] && !!l[1])
  for (let iter = 0; iter < 300; iter++) {
    const alpha = 1 - iter / 300
    for (let i = 0; i < pts.length; i++) {
      for (let j = i + 1; j < pts.length; j++) {
        const a = pts[i]!, b = pts[j]!
        let dx = a.x - b.x, dy = a.y - b.y
        const d2 = Math.max(dx * dx + dy * dy, 25)
        const f = (2200 / d2) * alpha
        const d = Math.sqrt(d2)
        dx /= d; dy /= d
        a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f
      }
    }
    for (const [a, b] of links) {
      const dx = b.x - a.x, dy = b.y - a.y
      const d = Math.sqrt(dx * dx + dy * dy) || 1
      const f = ((d - 90) / d) * 0.04 * alpha
      a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f
    }
    for (const p of pts) {
      p.vx += (width / 2 - p.x) * 0.004 * alpha
      p.vy += (height.value / 2 - p.y) * 0.004 * alpha
      if (p.id === props.root) { p.vx = 0; p.vy = 0; p.x = width / 2; p.y = height.value / 2 }
      p.x = Math.min(width - 20, Math.max(20, p.x + p.vx))
      p.y = Math.min(height.value - 20, Math.max(20, p.y + p.vy))
      p.vx *= 0.6; p.vy *= 0.6
    }
  }
  points.value = pts
}

watch(() => [props.nodes, props.edges], layout, { immediate: true })

const byId = computed(() => new Map(points.value.map((p) => [p.id, p])))
const lines = computed(() =>
  props.edges
    .map((e) => ({ e, a: byId.value.get(e.from), b: byId.value.get(e.to) }))
    .filter((l) => l.a && l.b),
)
// Built once per graph, so hovering a node looks up its neighbours instead
// of walking every edge.
const adjacency = computed(() => {
  const m = new Map<string, Set<string>>()
  const add = (a: string, b: string) => { let s = m.get(a); if (!s) m.set(a, (s = new Set())); s.add(b) }
  for (const e of props.edges) { add(e.from, e.to); add(e.to, e.from) }
  return m
})
const none = new Set<string>()
const neighbours = computed(() => (hovered.value && adjacency.value.get(hovered.value)) || none)

function color(n: ViewNode): string {
  if (n.id === props.root) return 'var(--accent)'
  if (n.is_test) return 'var(--success)'
  if (n.security_tags?.length) return 'var(--danger)'
  if (n.kind === 'InterfaceMethod') return 'var(--warning)'
  return 'var(--muted)'
}
function radius(n: ViewNode): number {
  return n.id === props.root ? 10 : 5 + Math.min(6, Math.sqrt(n.complexity ?? 1))
}
function label(name: string): string {
  const s = name.replace(/^.*\./, '')
  return s.length > 28 ? `${s.slice(0, 27)}…` : s
}
</script>

<template>
  <div class="canvas-wrap">
    <svg :viewBox="`0 0 ${width} ${height}`" role="group" aria-label="Call graph">
      <defs>
        <marker :id="arrow" viewBox="0 0 10 10" refX="16" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--line)" />
        </marker>
      </defs>
      <line
        v-for="(l, i) in lines" :key="i"
        :x1="l.a!.x" :y1="l.a!.y" :x2="l.b!.x" :y2="l.b!.y"
        :stroke="hovered && (l.e.from === hovered || l.e.to === hovered) ? 'var(--accent)' : 'var(--line)'"
        :stroke-dasharray="l.e.kind === 'DISPATCHES_TO' ? '4 3' : undefined"
        stroke-width="1.4" :marker-end="`url(#${arrow})`"
      />
      <g
        v-for="p in points" :key="p.id" class="node"
        :opacity="hovered && hovered !== p.id && !neighbours.has(p.id) ? 0.25 : 1"
        tabindex="0" role="button" :aria-label="`${p.n.name}${p.n.location ? `, ${p.n.location}` : ''}`"
        @mouseenter="hovered = p.id" @mouseleave="hovered = null" @focus="hovered = p.id" @blur="hovered = null"
        @click="emit('select', p.id)" @keydown.enter.prevent="emit('select', p.id)" @keydown.space.prevent="emit('select', p.id)"
      >
        <circle :cx="p.x" :cy="p.y" :r="radius(p.n)" :fill="color(p.n)" stroke="var(--panel)" stroke-width="2" />
        <text :x="p.x + radius(p.n) + 4" :y="p.y + 4">{{ label(p.n.name) }}</text>
        <title>{{ p.n.name }}{{ p.n.location ? `\n${p.n.location}` : '' }}</title>
      </g>
    </svg>
    <div class="legend muted">
      <span><i style="background: var(--accent)" />selected</span>
      <span><i style="background: var(--muted)" />code</span>
      <span><i style="background: var(--warning)" />interface</span>
      <span><i style="background: var(--danger)" />security-sensitive</span>
      <span><i style="background: var(--success)" />test</span>
      <span>dashed = interface dispatch</span>
    </div>
  </div>
</template>

<style scoped>
.canvas-wrap svg { width: 100%; height: auto; display: block; background: var(--panel-2); border-radius: var(--radius-xs); }
.node { cursor: pointer; }
/* SVG ignores box-shadow, so the focus ring is a stroke on the dot. */
.node:focus { outline: none; }
.node:focus-visible circle { stroke: var(--focus-ring); stroke-width: 3; }
.node text { font: 11px var(--font-mono); fill: var(--ink); paint-order: stroke; stroke: var(--panel-2); stroke-width: 3px; }
.legend { display: flex; flex-wrap: wrap; gap: 14px; font-size: 12px; margin-top: 8px; }
.legend i { display: inline-block; width: 9px; height: 9px; border-radius: 50%; margin-right: 5px; vertical-align: middle; }
</style>
