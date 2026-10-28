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
const notice = ref('')
const busy = ref(false)
// One change at a time: a second click while the first is on its way would
// race it, and the refresh after each shows what was really saved.
async function act(fn: () => Promise<unknown>, done: string) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    await fn()
    notice.value = done
  } catch (e) {
    error.value = plainError(e)
  } finally {
    busy.value = false
  }
  await refresh()
}
const newTeam = ref('')
const add = reactive<Record<string, string>>({})
const removing = ref<Team | null>(null)
const removingMember = ref<{ team: Team; id: string; name: string } | null>(null)
watch(() => org.value?.slug, () => {
  error.value = ''
  notice.value = ''
  newTeam.value = ''
  for (const k of Object.keys(add)) delete add[k]
})
const createTeam = () => act(async () => { await apiPost(base.value, { name: newTeam.value }); newTeam.value = '' }, 'Team created.')
const setRole = (t: Team, role: string) => act(() => apiPut(`${base.value}/${t.slug}/role`, { role }), `Role of ${t.name} saved.`)
const addMember = (t: Team) => act(async () => { await apiPut(`${base.value}/${t.slug}/members`, { email: add[t.id], relation: 'member' }); add[t.id] = '' }, `Member added to ${t.name}.`)
const setRelation = (t: Team, email: string, relation: string) => act(() => apiPut(`${base.value}/${t.slug}/members`, { email, relation }), 'Saved.')
async function removeMember() {
  const r = removingMember.value
  if (!r) return
  await act(() => apiDelete(`${base.value}/${r.team.slug}/members/${encodeURIComponent(r.id)}`), `${r.name} removed from ${r.team.name}.`)
  removingMember.value = null
}
async function deleteTeam() {
  const t = removing.value
  if (!t) return
  await act(() => apiDelete(`${base.value}/${t.slug}`), `Team ${t.name} deleted.`)
  removing.value = null
}
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <section class="panel">
        <h2>Teams in {{ org.name }}</h2>
        <p class="muted small">A team can grant an organisation role or repository access. Organisation roles do not grant code access; use the repository's <em>Access</em> page for that. Owners and admins manage teams; a team's maintainers manage its members.</p>
        <p v-if="loadError" class="notice notice--danger" role="alert">{{ plainError(loadError) }}</p>
        <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
        <p v-if="notice" class="notice notice--ok" role="status">{{ notice }}</p>
        <form v-if="canManage" class="row" @submit.prevent="createTeam">
          <div class="field grow"><label for="tname">New team</label><input id="tname" v-model="newTeam" required maxlength="80" placeholder="e.g. Platform, Security, Contractors"></div>
          <button type="submit" class="align-end" :disabled="busy">Create team</button>
        </form>
      </section>
      <p v-if="!data && !loadError" class="muted small" role="status">Loading teams…</p>
      <p v-if="data && !data.teams.length" class="empty"><strong>No teams yet</strong>Teams save you granting the same access person by person.</p>
      <section v-for="t in data?.teams ?? []" :key="t.id" class="panel">
        <div class="panel__header">
          <h2>{{ t.name }} <span class="muted small mono">{{ org.slug }}/{{ t.slug }}</span></h2>
          <button v-if="canManage" type="button" class="secondary tiny" :disabled="busy" @click="removing = t">Delete team</button>
        </div>
        <div class="field role">
          <label :for="`role-${t.id}`">Role in {{ org.name }}</label>
          <select :id="`role-${t.id}`" :value="t.role ?? ''" :disabled="!canManage || busy" @change="setRole(t, ($event.target as HTMLSelectElement).value)">
            <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option>
          </select>
        </div>
        <div v-if="t.members.length" class="table-wrap">
          <table>
            <thead><tr><th>Member</th><th>Place</th><th><span class="sr-only">Actions</span></th></tr></thead>
            <tbody>
              <tr v-for="m in t.members" :key="m.user.id">
                <td class="wrap">{{ m.user.name }} <span class="muted">{{ m.user.email }}</span></td>
                <td>
                  <select class="inline" :value="m.relation" :aria-label="`Place of ${m.user.name}`" :disabled="busy" @change="setRelation(t, m.user.email, ($event.target as HTMLSelectElement).value)">
                    <option value="member">Member</option><option value="maintainer">Maintainer</option>
                  </select>
                </td>
                <td class="num"><button type="button" class="secondary tiny" :disabled="busy" :aria-label="`Remove ${m.user.name} from ${t.name}`" @click="removingMember = { team: t, id: m.user.id, name: m.user.name }">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="muted small">No members yet.</p>
        <form class="row" @submit.prevent="addMember(t)">
          <div class="field grow"><label :for="`add-${t.id}`">Add a member of {{ org.name }} by email</label><input :id="`add-${t.id}`" v-model="add[t.id]" type="email" required placeholder="name@company.com"></div>
          <button type="submit" class="align-end secondary" :disabled="busy">Add</button>
        </form>
      </section>
    </template>
    <ConfirmDialog :open="!!removing" title="Delete this team?" confirm-label="Delete team" danger :busy="busy" @confirm="deleteTeam" @cancel="removing = null">
      <p>Members of {{ removing?.name }} lose everything the team gave them. Their own memberships stay.</p>
    </ConfirmDialog>
    <ConfirmDialog :open="!!removingMember" title="Remove this member?" confirm-label="Remove" danger :busy="busy" @confirm="removeMember" @cancel="removingMember = null">
      <p>{{ removingMember?.name }} loses what {{ removingMember?.team.name }} gave them. Their membership of the organisation stays.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.role { max-width: 360px; }
.wrap { overflow-wrap: anywhere; }
.inline { width: auto; min-height: 30px; padding: 3px 8px; }
</style>
