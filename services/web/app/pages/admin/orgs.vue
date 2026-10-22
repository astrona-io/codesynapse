<script setup lang="ts">
import type { AdminOrg } from '~/utils/types'

const session = useSession()
const { data, error } = useApiGet<{ orgs: AdminOrg[] }>('admin/orgs')
const q = ref('')
const shown = computed(() => (data.value?.orgs ?? []).filter((o) => !q.value || `${o.name} ${o.slug}`.toLowerCase().includes(q.value.toLowerCase())))
async function open(o: AdminOrg, to: string) {
  await session.switchOrg(o.slug)
  await navigateTo(to)
}
</script>

<template>
  <section class="panel">
    <div class="panel__header">
      <h2>Organisations <span class="muted">{{ data?.orgs.length ?? 0 }}</span></h2>
      <input v-model="q" class="search" placeholder="Filter" aria-label="Filter organisations">
    </div>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Organisation</th><th class="num">Members</th><th>Roles</th><th class="num">Repositories</th><th class="num">Teams</th><th>Created</th><th /></tr></thead>
        <tbody>
          <tr v-for="o in shown" :key="o.id">
            <td>{{ o.name }} <span class="muted mono small">{{ o.slug }}</span></td>
            <td class="num">{{ o.members }}</td>
            <td class="small muted">{{ Object.entries(o.roles).map(([r, n]) => `${n} ${r}`).join(' · ') }}</td>
            <td class="num">{{ o.repositories }}<span v-if="o.lost_access" class="badge badge--amber" :title="'Private repositories whose Git token stopped working'">{{ o.lost_access }} without access</span></td>
            <td class="num">{{ o.teams }}</td>
            <td>{{ new Date(o.created_at).toLocaleDateString() }}</td>
            <td class="num nowrap">
              <button class="secondary tiny" @click="open(o, '/settings/organisation')">Members</button>
              <button class="secondary tiny" @click="open(o, '/overview')">Open</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p class="muted small">“Open” switches you into the organisation as platform administrator; choose “All organisations” in the sidebar to come back.</p>
  </section>
</template>

<style scoped>
.search { width: 220px; min-height: 30px; padding: 4px 10px; }
.nowrap { white-space: nowrap; }
td .badge { margin-left: 6px; }
.nowrap button + button { margin-left: 6px; }
</style>
