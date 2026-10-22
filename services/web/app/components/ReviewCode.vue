<script setup lang="ts">
import type { ReviewComment } from '~/utils/types'

const props = defineProps<{ comment: ReviewComment; code?: CommentCode }>()

const end = computed(() => Math.max(props.comment.end_line ?? 0, props.comment.line ?? 0))
const replacement = computed(() => replacementCode(props.comment.suggestion))
const rows = computed<DiffRow[]>(() => {
  const lines = props.code?.lines
  const line = props.comment.line
  if (!lines?.length || !line) return []
  if (replacement.value) {
    const d = suggestionDiff(lines, line, end.value, replacement.value)
    if (d.length) return d
  }
  return lines.map((l) => ({ kind: ' ', old: l.n, text: l.text }))
})
const isDiff = computed(() => rows.value.some((r) => r.kind !== ' '))
const marked = (r: DiffRow) => !isDiff.value && r.old !== undefined && r.old >= (props.comment.line ?? 0) && r.old <= end.value
const where = computed(() => (props.code?.ref ? `@ ${props.code.ref.slice(0, 10)}` : 'working tree (may have changed since the review)'))
</script>

<template>
  <div v-if="code && (rows.length || code.url || code.error)" class="code">
    <header class="row tight">
      <p class="label">{{ isDiff ? 'Before → suggested' : 'Code' }}</p>
      <span v-if="rows.length" class="mono small dim">{{ where }}</span>
      <a v-if="code.url" :href="code.url" target="_blank" rel="noopener noreferrer" class="small open">Open lines on {{ forgeName(code.url) }} ↗</a>
    </header>
    <p v-if="code.error" class="muted small">Code unavailable: {{ code.error }}</p>
    <div v-if="rows.length" class="scroll">
      <table class="mono">
        <tbody>
          <tr v-for="(r, i) in rows" :key="i" :class="{ add: r.kind === '+', del: r.kind === '-', mark: marked(r) }">
            <td class="n">{{ r.old ?? '' }}</td>
            <td v-if="isDiff" class="n">{{ r.new ?? '' }}</td>
            <td class="sign">{{ r.kind === ' ' ? '' : r.kind }}</td>
            <td class="t">{{ r.text }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.code { display: grid; gap: 6px; }
.code header { flex-wrap: wrap; align-items: baseline; }
.label { margin: 0; }
.open { margin-left: auto; color: var(--cyan); }
.scroll { overflow-x: auto; border: 1px solid var(--line); border-radius: var(--radius-xs); background: var(--bg); }
table { width: 100%; border-collapse: collapse; font-size: 12px; line-height: 1.55; }
td { padding: 0 8px; white-space: pre; vertical-align: top; border: 0; }
.n { width: 1%; text-align: right; color: var(--dim); user-select: none; }
.sign { width: 1%; padding: 0 4px; user-select: none; }
.t { color: var(--ink); }
tr.del { background: var(--tint-pink); }
tr.del .sign { color: var(--pink); }
tr.add { background: var(--tint-green); }
tr.add .sign { color: var(--green); }
tr.mark { background: var(--tint-cyan); }
tr.mark .n { color: var(--cyan); }
</style>
