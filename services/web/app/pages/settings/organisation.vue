<script setup lang="ts">
import type { AccountOrg, Membership, OrgGateway, Role } from '~/utils/types'

const session = useSession()
const org = computed(() => session.current.value?.org)
const { data, refresh, error: loadError } = useApiGet<{ org: AccountOrg; members: Membership[] }>(() => (org.value ? `accounts/orgs/${org.value.slug}/members` : 'accounts/orgs/-/members'))
const canManage = computed(() => session.can('admin'))
const { data: policy, refresh: refreshPolicy } = useApiGet<AccountOrg>(() => `accounts/orgs/${org.value?.slug ?? '-'}/repo-policy`)
const defaultAccess = ref<'NONE' | 'READ_ONLY'>('NONE')
watch(policy, (value) => { defaultAccess.value = value?.default_repo_access ?? 'NONE' }, { immediate: true })
const policySaved = ref(false)
const { data: gateway, refresh: refreshGateway } = useApiGet<OrgGateway>(() => `accounts/orgs/${org.value?.slug ?? '-'}/gateway`)
const gatewaySaved = ref(false)
async function setGateway(change: { enabled?: boolean; local_first?: boolean }) {
  error.value = ''
  gatewaySaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/gateway`, change)
    gatewaySaved.value = true
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
  await refreshGateway()
}
const { data: memberRepos, refresh: refreshMemberRepos } = useApiGet<{ enabled: boolean }>(() => `accounts/orgs/${org.value?.slug ?? '-'}/member-repos`)
const memberReposSaved = ref(false)
async function setMemberRepos(enabled: boolean) {
  error.value = ''
  memberReposSaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/member-repos`, { enabled })
    memberReposSaved.value = true
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
  await refreshMemberRepos()
}
const recoverRepo = ref('')
const recoverUser = ref('')
const recovered = ref(false)
const { data: accessChanges, refresh: refreshAccessChanges } = useApiGet<{ orphaned_repositories: string[]; inactive_external_grants: number; malformed_grants: number }>(() => `accounts/orgs/${org.value?.slug ?? '-'}/access-changes`, { immediate: false })
watch(() => session.current.value, (membership) => { if (membership?.role === 'owner') refreshAccessChanges() }, { immediate: true })
async function recoverAdmin() {
  error.value = ''
  recovered.value = false
  try {
    await apiPost(`accounts/orgs/${org.value!.slug}/repos/${encodeURIComponent(recoverRepo.value)}/recover-admin`, { user: recoverUser.value })
    recovered.value = true
    await refreshAccessChanges()
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
}
async function savePolicy() {
  error.value = ''
  policySaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/repo-policy`, { default_repo_access: defaultAccess.value })
    await refreshPolicy()
    await session.load()
    policySaved.value = true
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
}
const roles: { id: Role; label: string; help: string }[] = [
  { id: 'guest', label: 'Guest', help: 'Needs an explicit repository grant; receives no default access' },
  { id: 'viewer', label: 'Viewer', help: 'Receives the organisation repository default unless a grant overrides it' },
  { id: 'member', label: 'Member', help: 'Can add repositories; existing repositories follow their own permissions' },
  { id: 'admin', label: 'Admin', help: 'Also manages members, Git tokens and settings' },
  { id: 'owner', label: 'Owner', help: 'Manages organisation ownership and can explicitly recover repository administration' },
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
    <p v-if="!org" class="notice">You are not a member of an organisation yet. Create one below. Platform administration does not grant access to repository content.</p>
    <section v-if="org" class="panel">
      <h2>Default repository access</h2>
      <p class="muted small">Applies to members without a user or team grant. Guests always need a grant. Organisation roles do not grant repository administration.</p>
      <form v-if="canManage" class="row" @submit.prevent="savePolicy">
        <div class="field"><label for="default-access">Default</label><select id="default-access" v-model="defaultAccess"><option value="NONE">No access</option><option value="READ_ONLY">Read only</option></select></div>
        <button type="submit" class="align-end">Save default</button>
      </form>
      <p v-else>{{ policy?.default_repo_access === 'READ_ONLY' ? 'Read only' : 'No access' }}</p>
      <p v-if="policySaved" class="notice notice--ok" role="status">Repository default saved.</p>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    </section>
    <section v-if="org" class="panel">
      <h2>Members’ own repositories</h2>
      <p class="muted small">Owners and admins add repositories for all of {{ org.name }}; those follow the default above and the grants. When this is on, other members can also add a repository to their own space: only they, and the people they share it with, can open it. Turning it off keeps what members already added.</p>
      <label v-if="canManage" class="check"><input type="checkbox" :checked="memberRepos?.enabled ?? true" :disabled="!memberRepos" @change="setMemberRepos(($event.target as HTMLInputElement).checked)"> Let members add their own repositories</label>
      <p v-else>{{ memberRepos?.enabled ?? true ? 'On' : 'Off' }}. Ask an owner or admin to change it.</p>
      <p v-if="memberReposSaved" class="notice notice--ok" role="status">Setting saved.</p>
    </section>
    <section v-if="org" class="panel">
      <h2>Agent gateway</h2>
      <p class="muted small">When this is on, API keys of {{ org.name }} can send their agents’ model requests through CodeSynapse. Requests wait their turn, may be answered by a local model, and every request is checked and audited. It is on until an owner or admin turns it off.</p>
      <p v-if="gateway && !gateway.available" class="notice">Server switch off: the operator started the API with <code>CODESYNAPSE_GATEWAY=false</code>, so requests are refused even when this is on.</p>
      <template v-if="canManage">
        <label class="check"><input type="checkbox" :checked="gateway?.enabled ?? true" :disabled="!gateway" @change="setGateway({ enabled: ($event.target as HTMLInputElement).checked })"> Let keys of this organisation use the gateway</label>
        <label class="check"><input type="checkbox" :checked="gateway?.local_first ?? true" :disabled="!gateway?.enabled" @change="setGateway({ local_first: ($event.target as HTMLInputElement).checked })"> Answer with the local model first</label>
        <p class="muted small">Turn this off if the local model is too small for your agents’ main model; quick requests still go to it.</p>
      </template>
      <p v-else>{{ gateway?.enabled ? 'On' : 'Off' }}{{ gateway?.enabled ? (gateway.local_first ? ', local model first' : ', local model for quick requests only') : '' }}. Ask an owner or admin to change it.</p>
      <p v-if="gatewaySaved" class="notice notice--ok" role="status">Gateway setting saved.</p>
    </section>
    <section v-if="org && session.can('owner')" class="panel">
      <h2>Recover repository administration</h2>
      <p class="muted small">Assign an existing organisation member as repository administrator. This records an audit event and gives that person access to the repository.</p>
      <p v-if="accessChanges?.inactive_external_grants" class="notice">{{ accessChanges.inactive_external_grants }} external grant(s) are inactive until the recipient belongs to this organisation.</p>
      <p v-if="accessChanges?.malformed_grants" class="notice">{{ accessChanges.malformed_grants }} malformed grant(s) need repair. Recovery keeps affected members denied except the administrator you assign.</p>
      <p v-if="accessChanges?.orphaned_repositories.length" class="notice">Repositories needing an administrator: {{ accessChanges.orphaned_repositories.join(', ') }}.</p>
      <form class="row" @submit.prevent="recoverAdmin">
        <div class="field grow"><label for="recover-repo">Repository name</label><input id="recover-repo" v-model="recoverRepo" required></div>
        <div class="field grow"><label for="recover-user">Member email</label><input id="recover-user" v-model="recoverUser" type="email" required></div>
        <button type="submit" class="align-end">Assign administrator</button>
      </form>
      <p v-if="recovered" class="notice notice--ok" role="status">Repository administrator assigned.</p>
    </section>
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
