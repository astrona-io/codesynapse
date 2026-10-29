<script setup lang="ts">
// An "i" that explains a term or a figure. The text opens on mouse hover, on
// keyboard focus and on a tap (touch screens have no hover), closes on
// Escape or a tap elsewhere, and is the button's accessible description
// whether it is open or not, so screen readers always get it.
const props = defineProps<{ term?: string; text?: string; label?: string }>()
const body = computed(() => props.text ?? (props.term ? GLOSSARY[props.term] : '') ?? '')
const id = useId()

const button = ref<HTMLButtonElement>()
const bubble = ref<HTMLElement>()
const hovered = ref(false)
const focused = ref(false)
const pinned = ref(false)
const open = computed(() => hovered.value || focused.value || pinned.value)

// The bubble is fixed to the viewport and kept 16px inside it, so it never
// widens the page at phone width or hides under a scrolling panel's edge.
const EDGE = 16
const GAP = 8
const pos = ref<{ left: number; top: number } | null>(null)
function place() {
  const b = button.value
  const t = bubble.value
  if (!b || !t) return
  const r = b.getBoundingClientRect()
  const vw = document.documentElement.clientWidth
  const left = Math.max(EDGE, Math.min(r.left + r.width / 2 - t.offsetWidth / 2, vw - EDGE - t.offsetWidth))
  const above = r.top - GAP - t.offsetHeight
  pos.value = { left, top: above >= EDGE ? above : r.bottom + GAP }
}

let leaveTimer: ReturnType<typeof setTimeout> | undefined
function enter(e: PointerEvent) {
  if (e.pointerType !== 'mouse') return
  clearTimeout(leaveTimer)
  hovered.value = true
}
// A short grace period lets the pointer cross the gap into the bubble.
function leave(e: PointerEvent) {
  if (e.pointerType !== 'mouse') return
  leaveTimer = setTimeout(() => { hovered.value = false }, 120)
}
function focus() {
  focused.value = button.value?.matches(':focus-visible') ?? false
}
function blur() {
  focused.value = false
  pinned.value = false
}
// The tip can sit inside a link (HealthCard): a tap opens it, not the link.
function toggle(e: MouseEvent) {
  e.preventDefault()
  e.stopPropagation()
  pinned.value = !pinned.value
}
function close() {
  clearTimeout(leaveTimer)
  hovered.value = false
  focused.value = false
  pinned.value = false
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') close()
}
function onPointer(e: PointerEvent) {
  const target = e.target as Node | null
  if (target && (button.value?.contains(target) || bubble.value?.contains(target))) return
  pinned.value = false
}

function listen(on: boolean) {
  if (on) {
    document.addEventListener('keydown', onKey)
    document.addEventListener('pointerdown', onPointer)
    window.addEventListener('scroll', place, true)
    window.addEventListener('resize', place)
    return
  }
  document.removeEventListener('keydown', onKey)
  document.removeEventListener('pointerdown', onPointer)
  window.removeEventListener('scroll', place, true)
  window.removeEventListener('resize', place)
}
watch(open, async (isOpen) => {
  listen(isOpen)
  if (!isOpen) {
    pos.value = null
    return
  }
  await nextTick()
  place()
})
onBeforeUnmount(() => {
  clearTimeout(leaveTimer)
  if (open.value) listen(false)
})
</script>

<template>
  <span class="info" @pointerenter="enter" @pointerleave="leave">
    <button
      ref="button" type="button" class="tip" :aria-label="label || 'More about this'" :aria-describedby="id"
      @focus="focus" @blur="blur" @click="toggle"
    >
      <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false">
        <circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" stroke-width="1.5" />
        <circle cx="8" cy="4.75" r="1" fill="currentColor" />
        <path d="M8 7.25v4.5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
      </svg>
    </button>
    <span
      v-show="open" :id="id" ref="bubble" role="tooltip" class="bubble"
      :style="pos ? { left: `${pos.left}px`, top: `${pos.top}px` } : { visibility: 'hidden' }"
    >{{ body }}</span>
  </span>
</template>

<style scoped>
.info { display: inline-flex; vertical-align: middle; margin-left: 5px; }
button.tip, button.tip:hover:not(:disabled) {
  display: inline-grid; place-items: center; width: 20px; height: 20px; min-height: 0; padding: 0;
  border: 0; border-radius: 50%; background: none; color: var(--muted); cursor: help;
}
button.tip:hover:not(:disabled) { color: var(--ink); }
button.tip:focus-visible { color: var(--ink); box-shadow: var(--focus); }
.bubble {
  position: fixed; left: 0; top: 0; z-index: 50; display: block;
  width: max-content; max-width: min(300px, calc(100vw - 32px));
  padding: 8px 10px; border-radius: var(--radius-xs); background: var(--panel-2); border: 1px solid var(--line-strong);
  color: var(--ink); font: 400 12.5px/1.45 var(--font-sans); text-transform: none; letter-spacing: 0; text-align: left;
  white-space: normal; overflow-wrap: anywhere; box-shadow: var(--shadow-pop);
}
/* Tips reuse the lowercase hint fragments; start them as a sentence. */
.bubble::first-letter { text-transform: uppercase; }
</style>
