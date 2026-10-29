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
const gatewayError = ref('')
async function setGateway(change: { enabled?: boolean; local_first?: boolean; local_exclude?: string[]; shorten_prompts?: boolean }) {
  gatewayError.value = ''
  gatewaySaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/gateway`, change)
    gatewaySaved.value = true
  } catch (err) {
    gatewayError.value = plainError(err)
  }
  await refreshGateway()
}
// The exclude list is edited one model per line. The checks mirror the API's
// (internal/modelpattern), which decides; these only explain a refusal early.
const excludeText = ref('')
watch(gateway, (value) => { excludeText.value = (value?.local_exclude ?? []).join('\n') }, { immediate: true })
const excludePattern = /^[a-z0-9.:_-]+\*?$/
const excludeLines = computed(() => [...new Set(excludeText.value.split('\n').map(l => l.trim().toLowerCase()).filter(Boolean))].sort())
const excludeProblem = computed(() => {
  const bad = excludeLines.value.find(l => l.length > 64 || !excludePattern.test(l))
  if (bad) return `“${bad.slice(0, 64)}” is not a model name or a prefix ending in *. Use a–z, 0–9, “.”, “:”, “_” and “-”, at most 64 characters.`
  if (excludeLines.value.length > 32) return `At most 32 models can be excluded; this list has ${excludeLines.value.length}.`
  return ''
})
const excludeChanged = computed(() => excludeLines.value.join('\n') !== (gateway.value?.local_exclude ?? []).join('\n'))
const { data: memberRepos, refresh: refreshMemberRepos } = useApiGet<{ enabled: boolean }>(() => `accounts/orgs/${org.value?.slug ?? '-'}/member-repos`)
const memberReposSaved = ref(false)
const memberReposError = ref('')
async function setMemberRepos(enabled: boolean) {
  memberReposError.value = ''
  memberReposSaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/member-repos`, { enabled })
    memberReposSaved.value = true
  } catch (err) {
    memberReposError.value = plainError(err)
  }
  await refreshMemberRepos()
}
const recoverRepo = ref('')
const recoverUser = ref('')
const recovered = ref(false)
const { data: accessChanges, refresh: refreshAccessChanges } = useApiGet<{ orphaned_repositories: string[]; inactive_external_grants: number; malformed_grants: number }>(() => `accounts/orgs/${org.value?.slug ?? '-'}/access-changes`, { immediate: false })
watch(() => session.current.value, (membership) => { if (membership?.role === 'owner') refreshAccessChanges() }, { immediate: true })
const recoverError = ref('')
const recovering = ref(false)
async function recoverAdmin() {
  if (recovering.value) return
  recovering.value = true
  recoverError.value = ''
  recovered.value = false
  try {
    await apiPost(`accounts/orgs/${org.value!.slug}/repos/${encodeURIComponent(recoverRepo.value)}/recover-admin`, { user: recoverUser.value })
    recovered.value = true
    await refreshAccessChanges()
  } catch (err) {
    recoverError.value = plainError(err)
  } finally {
    recovering.value = false
  }
}
const policyError = ref('')
const savingPolicy = ref(false)
async function savePolicy() {
  if (savingPolicy.value) return
  savingPolicy.value = true
  policyError.value = ''
  policySaved.value = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/repo-policy`, { default_repo_access: defaultAccess.value })
    await refreshPolicy()
    await session.load()
    policySaved.value = true
  } catch (err) {
    policyError.value = plainError(err)
  } finally {
    savingPolicy.value = false
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
const notice = ref('')
const busy = ref(false)
async function put(e: string, r: Role, done: string) {
  if (busy.value) return false
  busy.value = true
  error.value = ''
  notice.value = ''
  let ok = false
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/members`, { email: e, role: r })
    notice.value = done
    ok = true
  } catch (err) {
    error.value = plainError(err)
  } finally {
    busy.value = false
  }
  // Refresh either way: a refused role change leaves the select showing the
  // role that was not saved until the list is read again.
  await refresh()
  return ok
}
async function add() {
  if (await put(email.value, role.value, `Added ${email.value}.`)) email.value = ''
}
const changeRole = (m: Membership, r: Role) => put(m.user.email, r, `${m.user.name} is now ${r}.`)
const removing = ref<Membership | null>(null)
async function remove() {
  const m = removing.value
  if (!m || busy.value) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    await apiDelete(`accounts/orgs/${org.value!.slug}/members/${encodeURIComponent(m.user.id)}`)
    notice.value = `${m.user.name} removed.`
  } catch (err) {
    error.value = plainError(err)
  } finally {
    busy.value = false
    removing.value = null
  }
  await refresh()
}
const newOrg = ref('')
const created = ref('')
const createError = ref('')
const creating = ref(false)
async function createOrg() {
  if (creating.value) return
  creating.value = true
  createError.value = ''
  created.value = ''
  try {
    const o = await apiPost<AccountOrg>('accounts/orgs', { name: newOrg.value })
    newOrg.value = ''
    await session.load()
    await session.switchOrg(o.slug)
    created.value = o.name
  } catch (err) {
    createError.value = plainError(err)
  } finally {
    creating.value = false
  }
}
// Messages belong to the organisation they were about; the watch on the
// organisation clears them when the sidebar switches to another one. The
// "created" message is set after the switch, so it survives it.
watch(() => org.value?.slug, () => {
  for (const r of [error, notice, policyError, gatewayError, memberReposError, recoverError, createError]) r.value = ''
  policySaved.value = false
  gatewaySaved.value = false
  memberReposSaved.value = false
  recovered.value = false
  recoverRepo.value = ''
  recoverUser.value = ''
})
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">You are not a member of an organisation yet. Create one below. Platform administration does not grant access to repository content.</p>
    <section v-if="org" class="panel">
      <h2>Default repository access</h2>
      <p class="muted small">Applies to members without a user or team grant. Guests always need a grant. Organisation roles do not grant repository administration.</p>
      <form v-if="canManage" class="row" @submit.prevent="savePolicy">
        <div class="field"><label for="default-access">Default</label><select id="default-access" v-model="defaultAccess"><option value="NONE">No access</option><option value="READ_ONLY">Read only</option></select></div>
        <button type="submit" class="align-end" :disabled="savingPolicy">{{ savingPolicy ? 'Saving…' : 'Save default' }}</button>
      </form>
      <p v-else>{{ policy?.default_repo_access === 'READ_ONLY' ? 'Read only' : 'No access' }}</p>
      <p v-if="policySaved" class="notice notice--ok" role="status">Repository default saved.</p>
      <p v-if="policyError" class="notice notice--danger" role="alert">{{ policyError }}</p>
    </section>
    <section v-if="org" class="panel">
      <h2>Members’ own repositories</h2>
      <p class="muted small">Owners and admins add repositories for all of {{ org.name }}; those follow the default above and the grants. When this is on, other members can also add a repository to their own space: only they, and the people they share it with, can open it. Turning it off keeps what members already added.</p>
      <label v-if="canManage" class="check"><input type="checkbox" :checked="memberRepos?.enabled ?? true" :disabled="!memberRepos" @change="setMemberRepos(($event.target as HTMLInputElement).checked)"> Let members add their own repositories</label>
      <p v-else>{{ memberRepos?.enabled ?? true ? 'On' : 'Off' }}. Ask an owner or admin to change it.</p>
      <p v-if="memberReposSaved" class="notice notice--ok" role="status">Setting saved.</p>
      <p v-if="memberReposError" class="notice notice--danger" role="alert">{{ memberReposError }}</p>
    </section>
    <section v-if="org" class="panel">
      <h2>Agent gateway</h2>
      <p class="muted small">When this is on, API keys of {{ org.name }} can send their agents’ model requests through CodeSynapse. Requests wait their turn, may be answered by a local model, and every request is checked and audited. It is on until an owner or admin turns it off.</p>
      <p v-if="gateway && !gateway.available" class="notice">Server switch off: the operator started the API with <code>CODESYNAPSE_GATEWAY=false</code>, so requests are refused even when this is on.</p>
      <template v-if="canManage">
        <label class="check"><input type="checkbox" :checked="gateway?.enabled ?? true" :disabled="!gateway" @change="setGateway({ enabled: ($event.target as HTMLInputElement).checked })"> Let keys of this organisation use the gateway</label>
        <label class="check"><input type="checkbox" :checked="gateway?.local_first ?? true" :disabled="!gateway?.enabled" @change="setGateway({ local_first: ($event.target as HTMLInputElement).checked })"> Answer with the local model first</label>
        <p class="muted small">Turn this off if the local model is too small for your agents’ main model; quick requests still go to it.</p>
        <form class="field exclude" @submit.prevent="setGateway({ local_exclude: excludeLines })">
          <label for="gateway-exclude">Never answer these models locally</label>
          <textarea id="gateway-exclude" v-model="excludeText" rows="4" spellcheck="false" autocomplete="off" placeholder="claude-opus-*" :disabled="!gateway?.enabled" :aria-invalid="excludeProblem ? 'true' : undefined" aria-describedby="gateway-exclude-hint" />
          <span id="gateway-exclude-hint" class="hint">One model per line: an exact name, or a prefix ending in * (claude-opus-*). Requests for these models always go to the cloud, also when the local model answers first. Up to 32.</span>
          <p v-if="excludeProblem" class="notice notice--danger" role="alert">{{ excludeProblem }}</p>
          <button type="submit" class="secondary align-start" :disabled="!gateway?.enabled || !!excludeProblem || !excludeChanged">Save list</button>
        </form>
        <label class="check"><input type="checkbox" :checked="gateway?.shorten_prompts ?? true" :disabled="!gateway?.enabled" @change="setGateway({ shorten_prompts: ($event.target as HTMLInputElement).checked })"> Shorten typed prompts before they go to the cloud</label>
        <p class="muted small">On by default. The local model rewrites the prose people type into a terse version that keeps every name, number, path, quote and negation. Code, tool results, files and the agent’s own instructions are sent as they are, and models on the list above go out untouched. A rewrite is made once and reused for the rest of the chat, so the provider’s prompt cache keeps working.</p>
      </template>
      <p v-else>{{ gateway?.enabled ? 'On' : 'Off' }}{{ gateway?.enabled ? (gateway.local_first ? ', local model first' : ', local model for quick requests only') : '' }}{{ gateway?.enabled ? (gateway.shorten_prompts ? ', typed prompts shortened' : ', typed prompts sent as typed') : '' }}{{ gateway?.local_exclude?.length ? `; never locally: ${gateway.local_exclude.join(', ')}` : '' }}. Ask an owner or admin to change it.</p>
      <p v-if="gateway?.installation_exclude?.length" class="muted small">The operator also keeps these models away from the local model for every organisation: <code>{{ gateway.installation_exclude.join(', ') }}</code>.</p>
      <p v-if="gateway?.shorten_prompts && !gateway.shorten_available" class="notice">Prompts are not shortened yet: no local model is routed to <code>compress</code>, or the operator set <code>compress_prompts: false</code>. Requests go out as typed.</p>
      <p v-if="gatewaySaved" class="notice notice--ok" role="status">Gateway setting saved.</p>
      <p v-if="gatewayError" class="notice notice--danger" role="alert">{{ gatewayError }}</p>
    </section>
    <section v-if="org && session.can('owner')" class="panel">
      <h2>Recover repository administration</h2>
      <p class="muted small">Assign an existing organisation member as repository administrator. This records an audit event and gives that person access to the repository.</p>
      <p v-if="accessChanges?.inactive_external_grants" class="notice">{{ accessChanges.inactive_external_grants }} external grant(s) are inactive until the recipient belongs to this organisation.</p>
      <p v-if="accessChanges?.malformed_grants" class="notice">{{ accessChanges.malformed_grants }} malformed grant(s) need repair. Recovery keeps affected members denied except the administrator you assign.</p>
      <p v-if="accessChanges?.orphaned_repositories.length" class="notice">Repositories needing an administrator: {{ accessChanges.orphaned_repositories.join(', ') }}.</p>
      <form class="row" @submit.prevent="recoverAdmin">
        <div class="field grow"><label for="recover-repo">Repository name</label><input id="recover-repo" v-model="recoverRepo" required autocomplete="off" spellcheck="false"></div>
        <div class="field grow"><label for="recover-user">Member email</label><input id="recover-user" v-model="recoverUser" type="email" required></div>
        <button type="submit" class="align-end" :disabled="recovering">{{ recovering ? 'Assigning…' : 'Assign administrator' }}</button>
      </form>
      <p v-if="recovered" class="notice notice--ok" role="status">Repository administrator assigned.</p>
      <p v-if="recoverError" class="notice notice--danger" role="alert">{{ recoverError }}</p>
    </section>
    <section v-if="org" class="panel">
      <div class="panel__header"><h2>Members of {{ org.name }}</h2><span v-if="data" class="muted small">{{ data.members.length }} member(s)</span></div>
      <p v-if="loadError" class="notice notice--danger" role="alert">{{ plainError(loadError) }}</p>
      <p v-else-if="!data" class="muted small" role="status">Loading members…</p>
      <div v-if="data" class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Email</th><th>Role</th><th><span class="sr-only">Actions</span></th></tr></thead>
          <tbody>
            <tr v-for="m in data.members" :key="m.user.id">
              <td>{{ m.user.name }}<span v-if="m.user.id === session.me.value?.user.id" class="muted"> (you)</span></td>
              <td class="wrap">{{ m.user.email }}</td>
              <td>
                <select v-if="canManage" :value="m.role" :aria-label="`Role of ${m.user.name}`" class="role" :disabled="busy" @change="changeRole(m, ($event.target as HTMLSelectElement).value as Role)">
                  <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option>
                </select>
                <span v-else class="badge">{{ m.role }}</span>
              </td>
              <td class="num"><button v-if="canManage" type="button" class="secondary tiny" :disabled="busy" :aria-label="`Remove ${m.user.name}`" @click="removing = m">Remove</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <form v-if="canManage" class="row" @submit.prevent="add">
        <div class="field grow"><label for="memail">Add a member by email</label><input id="memail" v-model="email" type="email" placeholder="name@company.com" required></div>
        <div class="field"><label for="mrole">Role</label><select id="mrole" v-model="role"><option v-for="r in roles" :key="r.id" :value="r.id">{{ r.label }}</option></select></div>
        <button type="submit" class="align-end" :disabled="busy">Add</button>
      </form>
      <p v-if="canManage" class="muted small">People need an account first. Then they appear here with the role you chose.</p>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
      <p v-if="notice" class="notice notice--ok" role="status">{{ notice }}</p>
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
        <button type="submit" class="align-end" :disabled="creating">{{ creating ? 'Creating…' : 'Create' }}</button>
      </form>
      <p v-if="createError" class="notice notice--danger" role="alert">{{ createError }}</p>
      <p v-if="created" class="notice notice--ok" role="status">Created {{ created }}. You are working in it now.</p>
    </section>
    <ConfirmDialog :open="!!removing" title="Remove this member?" confirm-label="Remove" danger :busy="busy" @confirm="remove" @cancel="removing = null">
      <p>{{ removing?.user.name }} loses access to {{ org?.name }}, and their API keys for it stop working.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.align-start { align-self: flex-start; }
.exclude { margin-top: 12px; max-width: 520px; }
.exclude textarea { min-height: 96px; }
.wrap { overflow-wrap: anywhere; }
.role { width: auto; min-height: 30px; padding: 3px 8px; }
.roles { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; margin: 8px 0 0; }
.roles dd { margin: 0; }
</style>
