<script setup lang="ts">
import type { PermissionInfo, PermissionLevel, PermissionResource } from '~/utils/types'

// Picks permissions grouped by resource and area: one level per area,
// because a level includes the ones below it (full > write > read). The
// value is the chosen level of each area; the server expands it.
const props = defineProps<{
  catalog: PermissionInfo[]
  modelValue: string[]
  // Only this resource's permissions; both when absent.
  resource?: PermissionResource
  disabled?: boolean
  idPrefix: string
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string[]] }>()

const rank: Record<PermissionLevel, number> = { read: 1, write: 2, full: 3 }
const resourceLabel: Record<PermissionResource, string> = { repository: 'Repository', organisation: 'Organisation' }

const groups = computed(() => {
  const byResource = new Map<PermissionResource, Map<string, PermissionLevel[]>>()
  for (const p of props.catalog) {
    if (props.resource && p.resource !== props.resource) continue
    const areas = byResource.get(p.resource) ?? new Map<string, PermissionLevel[]>()
    areas.set(p.area, [...(areas.get(p.area) ?? []), p.level].sort((a, b) => rank[a] - rank[b]))
    byResource.set(p.resource, areas)
  }
  return [...byResource].map(([resource, areas]) => ({ resource, areas: [...areas].map(([area, levels]) => ({ area, levels })) }))
})

// The highest chosen level per resource.area.
const chosen = computed(() => {
  const top = new Map<string, PermissionLevel>()
  for (const p of props.modelValue) {
    const m = /^(\w+)\.(\w+):(read|write|full)$/.exec(p)
    if (!m) continue
    const key = `${m[1]}.${m[2]}`
    const level = m[3] as PermissionLevel
    if (!top.has(key) || rank[level] > rank[top.get(key)!]) top.set(key, level)
  }
  return top
})

function set(resource: string, area: string, level: string) {
  const next = new Map(chosen.value)
  const key = `${resource}.${area}`
  if (level) next.set(key, level as PermissionLevel)
  else next.delete(key)
  emit('update:modelValue', [...next].map(([k, l]) => `${k}:${l}`).sort())
}
const label = (area: string) => area.replace(/_/g, ' ')
</script>

<template>
  <div class="picker">
    <fieldset v-for="g in groups" :key="g.resource" class="group">
      <legend>{{ resourceLabel[g.resource] }}</legend>
      <div class="areas">
        <div v-for="a in g.areas" :key="a.area" class="area">
          <label :for="`${idPrefix}-${g.resource}-${a.area}`">{{ label(a.area) }}</label>
          <select :id="`${idPrefix}-${g.resource}-${a.area}`" :value="chosen.get(`${g.resource}.${a.area}`) ?? ''" :disabled="disabled" @change="set(g.resource, a.area, ($event.target as HTMLSelectElement).value)">
            <option value="">None</option>
            <option v-for="l in a.levels" :key="l" :value="l">{{ l }}</option>
          </select>
        </div>
      </div>
    </fieldset>
  </div>
</template>

<style scoped>
.picker { display: grid; gap: 12px; }
.group { border: 2px solid var(--line); border-radius: var(--radius-md); padding: 10px 14px 14px; margin: 0; background: var(--panel-2); }
legend { font-family: var(--font-display); font-weight: 600; padding: 0 6px; }
.areas { display: grid; grid-template-columns: repeat(auto-fill, minmax(170px, 1fr)); gap: 8px 12px; }
.area { display: grid; gap: 2px; }
.area label { font-size: 0.85rem; text-transform: capitalize; }
.area select { min-height: 32px; padding: 3px 8px; }
</style>
