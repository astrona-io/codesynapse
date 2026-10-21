<script setup lang="ts">
// Interactive canvas graph: force layout that keeps running while nodes are
// added, pan (drag background), zoom (wheel), drag nodes (pins them), click
// to select, double-click to expand. Canvas instead of SVG so a few hundred
// nodes stay smooth.
import type { ViewEdge, ViewNode } from '~/utils/types'

const props = defineProps<{
  nodes: ViewNode[]
  edges: ViewEdge[]
  selected?: string | null
  hiddenKinds: Set<string>
  hiddenEdges: Set<string>
}>()
const emit = defineEmits<{ select: [id: string | null]; expand: [id: string] }>()

interface Sim { id: string; x: number; y: number; vx: number; vy: number; pinned: boolean; n: ViewNode }

const canvas = ref<HTMLCanvasElement | null>(null)
const wrap = ref<HTMLDivElement | null>(null)
const sims = new Map<string, Sim>()
let view = { x: 0, y: 0, k: 1 }
let alpha = 1
let raf = 0
let size = { w: 800, h: 600 }
let palette: Record<string, string> = {}
const hovered = ref<string | null>(null)

const KIND_COLOR: Record<string, string> = {
  Function: '--k-func', Method: '--k-method', InterfaceMethod: '--k-iface', Type: '--k-type',
  External: '--k-ext', Package: '--k-pkg', File: '--k-file', Document: '--k-doc', Section: '--k-doc',
  Commit: '--k-commit', Author: '--k-commit', Repository: '--k-pkg',
  Finding: '--k-finding', Module: '--k-module', Vulnerability: '--k-vuln',
  Config: '--k-config', ConfigEntry: '--k-config', Template: '--k-template', TemplateDef: '--k-template',
}
const EDGE_COLOR: Record<string, string> = {
  CALLS: '--e-calls', DISPATCHES_TO: '--e-dispatch', IMPLEMENTS: '--e-dispatch', MENTIONS: '--e-doc',
  HAS_FINDING: '--k-finding', AFFECTS: '--k-vuln', INCLUDES: '--k-template', USES_VALUE: '--k-config',
}

function readPalette() {
  const cs = getComputedStyle(document.documentElement)
  const names = ['--text', '--muted', '--border', '--surface-2', '--accent', '--bad', '--good',
    ...new Set([...Object.values(KIND_COLOR), ...Object.values(EDGE_COLOR)])]
  palette = Object.fromEntries(names.map((n) => [n, cs.getPropertyValue(n).trim() || '#888']))
}

const visibleNodes = computed(() => props.nodes.filter((n) => !props.hiddenKinds.has(n.kind)))
const visibleIds = computed(() => new Set(visibleNodes.value.map((n) => n.id)))
const visibleEdges = computed(() =>
  props.edges.filter((e) => !props.hiddenEdges.has(e.kind) && visibleIds.value.has(e.from) && visibleIds.value.has(e.to)),
)
const degree = computed(() => {
  const d = new Map<string, number>()
  for (const e of visibleEdges.value) {
    d.set(e.from, (d.get(e.from) ?? 0) + 1)
    d.set(e.to, (d.get(e.to) ?? 0) + 1)
  }
  return d
})

// Keep simulation state for existing nodes; place new ones next to a
// connected node so expansion grows outward instead of exploding.
watch(() => [props.nodes, props.edges], () => {
  const ids = new Set(props.nodes.map((n) => n.id))
  for (const id of [...sims.keys()]) if (!ids.has(id)) sims.delete(id)
  const neighbour = new Map<string, string>()
  for (const e of props.edges) {
    if (sims.has(e.from) && !sims.has(e.to)) neighbour.set(e.to, e.from)
    if (sims.has(e.to) && !sims.has(e.from)) neighbour.set(e.from, e.to)
  }
  for (const n of props.nodes) {
    const existing = sims.get(n.id)
    if (existing) { existing.n = n; continue }
    const anchor = sims.get(neighbour.get(n.id) ?? props.selected ?? '')
    const a = Math.random() * Math.PI * 2
    const r = anchor ? 40 + Math.random() * 40 : 200 * Math.sqrt(Math.random())
    sims.set(n.id, {
      id: n.id, n, pinned: false, vx: 0, vy: 0,
      x: (anchor?.x ?? 0) + Math.cos(a) * r, y: (anchor?.y ?? 0) + Math.sin(a) * r,
    })
  }
  alpha = Math.max(alpha, 0.8)
  start()
}, { immediate: true })
watch(() => [props.hiddenKinds, props.hiddenEdges, props.selected], () => { alpha = Math.max(alpha, 0.3); start() })

function step() {
  const nodes = visibleNodes.value.map((n) => sims.get(n.id)).filter((s): s is Sim => !!s)
  const repulsion = 1800
  for (let i = 0; i < nodes.length; i++) {
    const a = nodes[i]!
    for (let j = i + 1; j < nodes.length; j++) {
      const b = nodes[j]!
      let dx = a.x - b.x, dy = a.y - b.y
      let d2 = dx * dx + dy * dy
      if (d2 > 250000) continue // ignore far pairs: O(n²) stays cheap
      if (d2 < 1) { dx = Math.random() - 0.5; dy = Math.random() - 0.5; d2 = 1 }
      const f = (repulsion / d2) * alpha
      const d = Math.sqrt(d2)
      a.vx += (dx / d) * f; a.vy += (dy / d) * f
      b.vx -= (dx / d) * f; b.vy -= (dy / d) * f
    }
  }
  for (const e of visibleEdges.value) {
    const a = sims.get(e.from), b = sims.get(e.to)
    if (!a || !b) continue
    const dx = b.x - a.x, dy = b.y - a.y
    const d = Math.sqrt(dx * dx + dy * dy) || 1
    const f = ((d - 70) / d) * 0.05 * alpha
    a.vx += dx * f; a.vy += dy * f; b.vx -= dx * f; b.vy -= dy * f
  }
  for (const s of nodes) {
    s.vx -= s.x * 0.002 * alpha
    s.vy -= s.y * 0.002 * alpha
    if (s.pinned) { s.vx = 0; s.vy = 0; continue }
    s.x += Math.max(-30, Math.min(30, s.vx))
    s.y += Math.max(-30, Math.min(30, s.vy))
    s.vx *= 0.55; s.vy *= 0.55
  }
  alpha *= 0.985
}

function radius(s: Sim): number {
  return 4 + Math.min(9, Math.sqrt(degree.value.get(s.id) ?? 0) * 1.6)
}

function draw() {
  const c = canvas.value
  if (!c) return
  const ctx = c.getContext('2d')
  if (!ctx) return
  const dpr = window.devicePixelRatio || 1
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  ctx.fillStyle = palette['--surface-2']!
  ctx.fillRect(0, 0, size.w, size.h)
  ctx.translate(size.w / 2 + view.x, size.h / 2 + view.y)
  ctx.scale(view.k, view.k)

  const focus = hovered.value ?? props.selected ?? null
  const focusSet = new Set<string>()
  if (focus) {
    focusSet.add(focus)
    for (const e of visibleEdges.value) {
      if (e.from === focus) focusSet.add(e.to)
      if (e.to === focus) focusSet.add(e.from)
    }
  }

  ctx.lineWidth = 1 / view.k
  for (const e of visibleEdges.value) {
    const a = sims.get(e.from), b = sims.get(e.to)
    if (!a || !b) continue
    const active = focus && (e.from === focus || e.to === focus)
    ctx.globalAlpha = focus && !active ? 0.08 : active ? 0.9 : 0.35
    ctx.strokeStyle = palette[EDGE_COLOR[e.kind] ?? '--border'] ?? palette['--border']!
    ctx.setLineDash(e.kind === 'DISPATCHES_TO' ? [4 / view.k, 3 / view.k] : [])
    ctx.beginPath()
    ctx.moveTo(a.x, a.y)
    ctx.lineTo(b.x, b.y)
    ctx.stroke()
    // Arrowhead at the target's rim.
    const dx = b.x - a.x, dy = b.y - a.y
    const d = Math.sqrt(dx * dx + dy * dy) || 1
    const r = radius(b) + 2
    const tx = b.x - (dx / d) * r, ty = b.y - (dy / d) * r
    const s = 5 / view.k
    ctx.beginPath()
    ctx.moveTo(tx, ty)
    ctx.lineTo(tx - (dx / d) * s - (dy / d) * s * 0.6, ty - (dy / d) * s + (dx / d) * s * 0.6)
    ctx.lineTo(tx - (dx / d) * s + (dy / d) * s * 0.6, ty - (dy / d) * s - (dx / d) * s * 0.6)
    ctx.closePath()
    ctx.fillStyle = ctx.strokeStyle
    ctx.fill()
  }
  ctx.setLineDash([])

  for (const n of visibleNodes.value) {
    const s = sims.get(n.id)
    if (!s) continue
    const r = radius(s)
    ctx.globalAlpha = focus && !focusSet.has(n.id) ? 0.18 : 1
    ctx.beginPath()
    ctx.arc(s.x, s.y, r, 0, Math.PI * 2)
    ctx.fillStyle = palette[KIND_COLOR[n.kind] ?? '--muted']!
    ctx.fill()
    if (n.security_tags?.length) {
      ctx.lineWidth = 2.2 / view.k
      ctx.strokeStyle = palette['--bad']!
      ctx.stroke()
    }
    if (n.id === props.selected) {
      ctx.lineWidth = 3 / view.k
      ctx.strokeStyle = palette['--accent']!
      ctx.beginPath()
      ctx.arc(s.x, s.y, r + 4 / view.k, 0, Math.PI * 2)
      ctx.stroke()
    }
    if (s.pinned) {
      ctx.fillStyle = palette['--text']!
      ctx.beginPath()
      ctx.arc(s.x, s.y, 1.6 / view.k, 0, Math.PI * 2)
      ctx.fill()
    }
  }

  // Labels: always for focus and its neighbours, otherwise only when zoomed
  // in enough or the node is a hub.
  ctx.font = `${11 / view.k}px 'JetBrains Mono', ui-monospace, monospace`
  ctx.textBaseline = 'middle'
  for (const n of visibleNodes.value) {
    const s = sims.get(n.id)
    if (!s) continue
    const show = focusSet.has(n.id) || view.k > 1.4 || (degree.value.get(n.id) ?? 0) >= 6
    if (!show || (focus && !focusSet.has(n.id))) continue
    const label = n.name.length > 40 ? `${n.name.slice(0, 39)}…` : n.name
    ctx.globalAlpha = 1
    ctx.lineWidth = 3 / view.k
    ctx.strokeStyle = palette['--surface-2']!
    ctx.strokeText(label, s.x + radius(s) + 3 / view.k, s.y)
    ctx.fillStyle = palette['--text']!
    ctx.fillText(label, s.x + radius(s) + 3 / view.k, s.y)
  }
  ctx.globalAlpha = 1
}

function frame() {
  if (alpha > 0.01) step()
  draw()
  raf = alpha > 0.01 || dragging ? requestAnimationFrame(frame) : 0
}
function start() {
  if (!raf && typeof window !== 'undefined') raf = requestAnimationFrame(frame)
}
function redraw() {
  if (!raf) requestAnimationFrame(draw)
}

// --- interaction ---------------------------------------------------------
let dragging: { kind: 'pan' | 'node'; sim?: Sim; sx: number; sy: number; moved: boolean; ox: number; oy: number } | null = null

function toWorld(ev: PointerEvent | WheelEvent | MouseEvent) {
  const rect = canvas.value!.getBoundingClientRect()
  return {
    x: (ev.clientX - rect.left - size.w / 2 - view.x) / view.k,
    y: (ev.clientY - rect.top - size.h / 2 - view.y) / view.k,
  }
}
function hit(ev: PointerEvent | MouseEvent): Sim | null {
  const p = toWorld(ev)
  let best: Sim | null = null
  let bestD = Infinity
  for (const n of visibleNodes.value) {
    const s = sims.get(n.id)
    if (!s) continue
    const d = Math.hypot(s.x - p.x, s.y - p.y)
    if (d < radius(s) + 4 / view.k && d < bestD) { best = s; bestD = d }
  }
  return best
}
function onDown(ev: PointerEvent) {
  canvas.value!.setPointerCapture(ev.pointerId)
  const s = hit(ev)
  dragging = s
    ? { kind: 'node', sim: s, sx: ev.clientX, sy: ev.clientY, moved: false, ox: s.x, oy: s.y }
    : { kind: 'pan', sx: ev.clientX, sy: ev.clientY, moved: false, ox: view.x, oy: view.y }
}
function onMove(ev: PointerEvent) {
  if (!dragging) {
    const s = hit(ev)
    const id = s?.id ?? null
    if (id !== hovered.value) { hovered.value = id; redraw() }
    canvas.value!.style.cursor = s ? 'pointer' : 'grab'
    return
  }
  const dx = ev.clientX - dragging.sx, dy = ev.clientY - dragging.sy
  if (Math.abs(dx) + Math.abs(dy) > 3) dragging.moved = true
  if (dragging.kind === 'pan') {
    view.x = dragging.ox + dx
    view.y = dragging.oy + dy
    redraw()
  } else if (dragging.sim && dragging.moved) {
    dragging.sim.x = dragging.ox + dx / view.k
    dragging.sim.y = dragging.oy + dy / view.k
    dragging.sim.pinned = true
    alpha = Math.max(alpha, 0.25)
    start()
  }
}
function onUp() {
  if (dragging && !dragging.moved) emit('select', dragging.kind === 'node' ? dragging.sim!.id : null)
  dragging = null
}
function onDblClick(ev: MouseEvent) {
  const s = hit(ev)
  if (s) emit('expand', s.id)
}
function onWheel(ev: WheelEvent) {
  ev.preventDefault()
  const rect = canvas.value!.getBoundingClientRect()
  const mx = ev.clientX - rect.left - size.w / 2, my = ev.clientY - rect.top - size.h / 2
  const k = Math.min(6, Math.max(0.1, view.k * Math.exp(-ev.deltaY * 0.0015)))
  view.x = mx - ((mx - view.x) * k) / view.k
  view.y = my - ((my - view.y) * k) / view.k
  view.k = k
  redraw()
}
function unpin(id: string) {
  const s = sims.get(id)
  if (s) { s.pinned = false; alpha = Math.max(alpha, 0.3); start() }
}

function fit() {
  const pts = visibleNodes.value.map((n) => sims.get(n.id)).filter((s): s is Sim => !!s)
  if (!pts.length) return
  const xs = pts.map((p) => p.x), ys = pts.map((p) => p.y)
  const minX = Math.min(...xs), maxX = Math.max(...xs), minY = Math.min(...ys), maxY = Math.max(...ys)
  const k = Math.min(3, 0.9 * Math.min(size.w / Math.max(1, maxX - minX), size.h / Math.max(1, maxY - minY)))
  view = { k, x: (-(minX + maxX) / 2) * k, y: (-(minY + maxY) / 2) * k }
  redraw()
}
function centerOn(id: string) {
  const s = sims.get(id)
  if (!s) return
  view.x = -s.x * view.k
  view.y = -s.y * view.k
  redraw()
}
defineExpose({ fit, centerOn, unpin })

function resize() {
  const c = canvas.value, w = wrap.value
  if (!c || !w) return
  const dpr = window.devicePixelRatio || 1
  size = { w: w.clientWidth, h: w.clientHeight }
  c.width = size.w * dpr
  c.height = size.h * dpr
  c.style.width = `${size.w}px`
  c.style.height = `${size.h}px`
  redraw()
}

let ro: ResizeObserver | undefined
let mq: MediaQueryList | undefined
const onScheme = () => { readPalette(); redraw() }
onMounted(() => {
  readPalette()
  resize()
  ro = new ResizeObserver(resize)
  ro.observe(wrap.value!)
  mq = window.matchMedia('(prefers-color-scheme: dark)')
  mq.addEventListener('change', onScheme)
  start()
  setTimeout(fit, 900)
})
onBeforeUnmount(() => {
  cancelAnimationFrame(raf)
  ro?.disconnect()
  mq?.removeEventListener('change', onScheme)
})
</script>

<template>
  <div ref="wrap" class="explorer-canvas">
    <canvas
      ref="canvas"
      @pointerdown="onDown" @pointermove="onMove" @pointerup="onUp" @pointercancel="onUp"
      @dblclick="onDblClick" @wheel="onWheel"
    />
  </div>
</template>

<style scoped>
.explorer-canvas { position: relative; width: 100%; height: 100%; min-height: 420px; border-radius: 8px; overflow: hidden; }
canvas { display: block; touch-action: none; cursor: grab; }
</style>
