<script setup lang="ts">
import type { Role, Team } from '~/utils/types'

const session = useSession()
const org = computed(() => session.current.value?.org)
const { data, refresh, error: loadError } = useApiGet<{ teams: Team[] }>(() => (org.value ? `accounts/orgs/${org.value.slug}/teams` : 'accounts/orgs/-/teams'))
const canManage = computed(() => session.can('admin'))
const base = computed(() => `accounts/orgs/${org.value?.slug}/teams`)
const roles: { id: Role | ''; label: string }[] = [
  { id: '', label: 'No organisation role' },
  { id: 'viewer', label: 'Viewer: reads everything' },
  { id: 'member', label: 'Member: also syncs and runs tasks' },
  { id: 'admin', label: 'Admin: also manages access' },
]
const error = ref('')
async function act(fn: () => Promise<unknown>) {
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
  await refresh()
}
const newTeam = ref('')
const add = reactive<Record<string, string>>({})
const removing = ref<Team | null>(null)
const createTeam = () => act(async () => { await apiPost(base.value, { name: newTeam.value }); newTeam.value = '' })
const setRole = (t: Team, role: string) => act(() => $fetch(apiPath(`${base.value}/${t.slug}/role`), { method: 'PUT', body: { role } }))
const addMember = (t: Team) => act(async () => { await $fetch(apiPath(`${base.value}/${t.slug}/members`), { method: 'PUT', body: { email: add[t.id], relation: 'member' } }); add[t.id] = '' })
const setRelation = (t: Team, email: string, relation: string) => act(() => $fetch(apiPath(`${base.value}/${t.slug}/members`), { method: 'PUT', body: { email, relation } }))
const removeMember = (t: Team, userID: string) => act(() => $fetch(apiPath(`${base.value}/${t.slug}/members/${userID}`), { method: 'DELETE' }))
async function deleteTeam() {
  const t = removing.value
  removing.value = null
  if (t) await act(() => $fetch(apiPath(`${base.value}/${t.slug}`), { method: 'DELETE' }))
}
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <section class="panel">
        <h2>Teams in {{ org.name }}</h2>
        <p class="muted small">A team can grant an organisation role or repository access. Organisation roles do not grant code access; use the repository's <em>Access</em> page for that. Owners and admins manage teams; a team's maintainers manage its members.</p>
        <p v-if="loadError" class="notice notice--danger">{{ apiErrorMessage(loadError) }}</p>
        <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
        <form v-if="canManage" class="row" @submit.prevent="createTeam">
          <div class="field grow"><label for="tname">New team</label><input id="tname" v-model="newTeam" required maxlength="80" placeholder="e.g. Platform, Security, Contractors"></div>
          <button type="submit" class="align-end">Create team</button>
        </form>
      </section>
      <p v-if="data && !data.teams.length" class="empty"><strong>No teams yet</strong>Teams save you granting the same access person by person.</p>
      <section v-for="t in data?.teams ?? []" :key="t.id" class="panel">
        <div class="panel__header">
          <h2>{{ t.name }} <span class="muted small mono">{{ org.slug }}/{{ t.slug }}</span></h2>
          <button v-if="canManage" class="secondary tiny" @click="removing = t">Delete team</button>
        </div>
        <div class="field role">
          <label :for="`role-${t.id}`">Role in {{ org.name }}</label>
          <select :id="`role-${t.id}`" :value="t.role ?? ''" :disabled="!canManage" @change="setRole(t, ($event.target as HTMLSelectElement).value)">
            <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option>
          </select>
        </div>
        <div v-if="t.members.length" class="table-wrap">
          <table>
            <thead><tr><th>Member</th><th>Place</th><th /></tr></thead>
            <tbody>
              <tr v-for="m in t.members" :key="m.user.id">
                <td>{{ m.user.name }} <span class="muted">{{ m.user.email }}</span></td>
                <td>
                  <select class="inline" :value="m.relation" :aria-label="`Place of ${m.user.name}`" @change="setRelation(t, m.user.email, ($event.target as HTMLSelectElement).value)">
                    <option value="member">Member</option><option value="maintainer">Maintainer</option>
                  </select>
                </td>
                <td class="num"><button class="secondary tiny" @click="removeMember(t, m.user.id)">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="muted small">No members yet.</p>
        <form class="row" @submit.prevent="addMember(t)">
          <div class="field grow"><label :for="`add-${t.id}`">Add a member of {{ org.name }} by email</label><input :id="`add-${t.id}`" v-model="add[t.id]" type="email" required placeholder="name@company.com"></div>
          <button type="submit" class="align-end secondary">Add</button>
        </form>
      </section>
    </template>
    <ConfirmDialog :open="!!removing" title="Delete this team?" confirm-label="Delete team" danger @confirm="deleteTeam" @cancel="removing = null">
      <p>Members of {{ removing?.name }} lose everything the team gave them. Their own memberships stay.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.role { max-width: 360px; }
.inline { width: auto; min-height: 30px; padding: 3px 8px; }
</style>
