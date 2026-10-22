<script setup lang="ts">
import type { AccountOrg, Membership, Role } from '~/utils/types'

const session = useSession()
const org = computed(() => session.current.value?.org)
const { data, refresh, error: loadError } = useApiGet<{ org: AccountOrg; members: Membership[] }>(() => (org.value ? `accounts/orgs/${org.value.slug}/members` : 'accounts/orgs/-/members'))
const canManage = computed(() => session.can('admin'))
const roles: { id: Role; label: string; help: string }[] = [
  { id: 'guest', label: 'Guest', help: 'Sees only repositories shared with them (contractors, auditors)' },
  { id: 'viewer', label: 'Viewer', help: 'Reads every repository, report and statistic of the organisation' },
  { id: 'member', label: 'Member', help: 'Also syncs, adds repositories and runs AI tasks' },
  { id: 'admin', label: 'Admin', help: 'Also manages members, Git tokens and settings' },
  { id: 'owner', label: 'Owner', help: 'Everything, including other owners' },
]
const email = ref('')
const role = ref<Role>('member')
const error = ref('')
async function put(e: string, r: Role) {
  error.value = ''
  try {
    await $fetch(apiPath(`accounts/orgs/${org.value!.slug}/members`), { method: 'PUT', body: { email: e, role: r } })
    await refresh()
  } catch (err) {
    error.value = apiErrorMessage(err)
    await refresh()
  }
}
async function add() {
  await put(email.value, role.value)
  if (!error.value) email.value = ''
}
const removing = ref<Membership | null>(null)
async function remove() {
  const m = removing.value
  removing.value = null
  if (!m) return
  error.value = ''
  try {
    await $fetch(apiPath(`accounts/orgs/${org.value!.slug}/members/${m.user.id}`), { method: 'DELETE' })
    await refresh()
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
}
const newOrg = ref('')
const created = ref('')
async function createOrg() {
  error.value = ''
  try {
    const o = await apiPost<AccountOrg>('accounts/orgs', { name: newOrg.value })
    created.value = o.name
    newOrg.value = ''
    await session.load()
    await session.switchOrg(o.slug)
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
}
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">{{ session.allOrgs.value ? 'You are viewing all organisations as platform administrator. Choose one in the sidebar to manage its members.' : 'You are not a member of an organisation yet. Create one below.' }}</p>
    <section v-if="org" class="panel">
      <div class="panel__header"><h2>Members of {{ org.name }}</h2><span class="muted small">{{ data?.members.length ?? 0 }} member(s)</span></div>
      <p v-if="loadError" class="notice notice--danger">{{ apiErrorMessage(loadError) }}</p>
      <div v-if="data" class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Email</th><th>Role</th><th /></tr></thead>
          <tbody>
            <tr v-for="m in data.members" :key="m.user.id">
              <td>{{ m.user.name }}<span v-if="m.user.id === session.me.value?.user.id" class="muted"> (you)</span></td>
              <td>{{ m.user.email }}</td>
              <td>
                <select v-if="canManage" :value="m.role" :aria-label="`Role of ${m.user.name}`" class="role" @change="put(m.user.email, ($event.target as HTMLSelectElement).value as Role)">
                  <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option>
                </select>
                <span v-else class="badge">{{ m.role }}</span>
              </td>
              <td class="num"><button v-if="canManage" class="secondary tiny" @click="removing = m">Remove</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <form v-if="canManage" class="row" @submit.prevent="add">
        <div class="field grow"><label for="memail">Add a member by email</label><input id="memail" v-model="email" type="email" placeholder="name@company.com" required></div>
        <div class="field"><label for="mrole">Role</label><select id="mrole" v-model="role"><option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option></select></div>
        <button type="submit" class="align-end">Add</button>
      </form>
      <p v-if="canManage" class="muted small">People need an account first. Then they appear here with the role you chose.</p>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
      <details>
        <summary class="muted small">What the roles can do</summary>
        <dl class="roles"><template v-for="r in roles" :key="r.id"><dt>{{ r.label }}</dt><dd class="muted">{{ r.help }}</dd></template></dl>
      </details>
    </section>
    <section class="panel">
      <h2>New organisation</h2>
      <p class="muted small">Organisations keep repositories, agent memory, reviews and statistics apart. You become its owner.</p>
      <form class="row" @submit.prevent="createOrg">
        <div class="field grow"><label for="oname">Name</label><input id="oname" v-model="newOrg" required maxlength="80" placeholder="e.g. Platform Team"></div>
        <button type="submit" class="align-end">Create</button>
      </form>
      <p v-if="created" class="notice notice--ok" role="status">Created {{ created }}. You are working in it now.</p>
    </section>
    <ConfirmDialog :open="!!removing" title="Remove this member?" confirm-label="Remove" danger @confirm="remove" @cancel="removing = null">
      <p>{{ removing?.user.name }} loses access to {{ org?.name }}, and their API keys for it stop working.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.role { width: auto; min-height: 30px; padding: 3px 8px; }
.roles { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; margin: 8px 0 0; }
.roles dd { margin: 0; }
</style>
