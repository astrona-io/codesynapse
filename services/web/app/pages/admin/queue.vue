<script setup lang="ts">
import type { AdminOrg } from '~/utils/types'

// The model queue across every organisation, or one of them.
useHead({ title: 'Model queue · Administration · CodeSynapse' })
const org = ref('')
const { data: orgs } = useApiGet<{ orgs: AdminOrg[] }>('admin/orgs')
</script>

<template>
  <div class="stack">
    <div class="row filters">
      <label class="label" for="queue-org">Organisation</label>
      <select id="queue-org" v-model="org" class="inline">
        <option value="">All organisations</option>
        <option v-for="o in orgs?.orgs ?? []" :key="o.id" :value="o.id">{{ o.name }}</option>
      </select>
    </div>
    <ModelQueue :key="org" scope="platform" :org="org || undefined" />
  </div>
</template>

<style scoped>
.filters { gap: 8px; }
</style>
