<script setup lang="ts">
import type { CustomRole, OrgRoleGrant, PermissionInfo, PermissionResource } from '~/utils/types'

const session = useSession()
const org = computed(() => session.current.value?.org)
const base = computed(() => `accounts/orgs/${org.value?.slug ?? '-'}`)
const { data, refresh, error: loadError } = useApiGet<{ roles: CustomRole[]; limit: number }>(() => `${base.value}/roles`)
const { data: grantsData, refresh: refreshGrants } = useApiGet<{ grants: OrgRoleGrant[] }>(() => `${base.value}/role-grants`)
const { data: catalogData } = useApiGet<{ permissions: PermissionInfo[] }>('accounts/permissions')
const catalog = computed(() => catalogData.value?.permissions ?? [])
// The API decides who may change roles (organisation.roles:full, signed in);
// the page offers the forms to full members and owners.
const canManage = computed(() => session.can('full'))
const roles = computed(() => data.value?.roles ?? [])
const orgRoles = computed(() => roles.value.filter((r) => r.resource === 'organisation'))
const resourceLabel: Record<PermissionResource, string> = { repository: 'Repository role', organisation: 'Organisation role' }

const error = ref('')
const notice = ref('')
const busy = ref(false)
async function act(fn: () => Promise<unknown>, done: string) {
  if (busy.value) return false
  busy.value = true
  error.value = ''
  notice.value = ''
  let ok = false
  try {
    await fn()
    notice.value = done
    ok = true
  } catch (e) {
    error.value = plainError(e)
  } finally {
    busy.value = false
  }
  await Promise.all([refresh(), refreshGrants()])
  return ok
}

// One form creates a role or edits the one picked; a role keeps its resource.
const editing = ref<CustomRole | null>(null)
const form = reactive({ name: '', description: '', resource: 'repository' as PermissionResource, permissions: [] as string[] })
function reset() {
  editing.value = null
  Object.assign(form, { name: '', description: '', resource: 'repository', permissions: [] })
}
function edit(r: CustomRole) {
  editing.value = r
  Object.assign(form, { name: r.name, description: r.description, resource: r.resource, permissions: [...r.permissions] })
}
watch(() => form.resource, () => { if (!editing.value) form.permissions = [] })
watch(() => org.value?.slug, () => { reset(); error.value = ''; notice.value = '' })
async function save() {
  const body = { ...form }
  const r = editing.value
  const ok = r
    ? await act(() => apiPut(`${base.value}/roles/${encodeURIComponent(r.id)}`, body), `Role ${form.name} saved.`)
    : await act(() => apiPost(`${base.value}/roles`, body), `Role ${form.name} created.`)
  if (ok) reset()
}
const deleting = ref<CustomRole | null>(null)
async function remove() {
  const r = deleting.value
  if (!r) return
  await act(() => apiDelete(`${base.value}/roles/${encodeURIComponent(r.id)}`), `Role ${r.name} deleted.`)
  deleting.value = null
  if (editing.value?.id === r.id) reset()
}

// The highest level per area, for the list.
function summary(r: CustomRole) {
  const order = ['read', 'write', 'full']
  const top = new Map<string, string>()
  for (const p of r.permissions) {
    const [area, level] = p.replace(/^\w+\./, '').split(':')
    if (area && level && order.indexOf(level) > order.indexOf(top.get(area) ?? '')) top.set(area, level)
  }
  return [...top].map(([a, l]) => `${a.replace(/_/g, ' ')}: ${l}`)
}

// Organisation roles add to a member's built-in role.
const kind = ref<'user' | 'team'>('user')
const who = ref('')
const grantRole = ref('')
async function assign() {
  const body: Record<string, string> = { role: grantRole.value }
  body[kind.value] = kind.value === 'team' && !who.value.includes('/') ? `${org.value!.slug}/${who.value}` : who.value
  if (await act(() => apiPut(`${base.value}/role-grants`, body), 'Role given.')) who.value = ''
}
const revoke = (g: OrgRoleGrant) => act(() => apiDelete(`${base.value}/role-grants?subject=${encodeURIComponent(`${g.subject_type}:${g.subject_id}`)}`), `${g.role.name} taken from ${g.label}.`)
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <section class="panel">
        <h2>Custom roles in {{ org.name }}</h2>
        <p class="muted small">A custom role is a named set of permissions. Give a <strong>repository role</strong> to a person or team on a repository's <em>Access</em> page; it replaces their built-in role there. Give an <strong>organisation role</strong> below; it adds to their built-in role. Nobody can make a role holding more than they hold, and built-in roles do not change.</p>
        <p v-if="loadError" class="notice notice--danger" role="alert">{{ plainError(loadError) }}</p>
        <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
        <p v-if="notice" class="notice notice--ok" role="status">{{ notice }}</p>
        <div v-if="roles.length" class="table-wrap">
          <table>
            <thead><tr><th>Role</th><th>Kind</th><th>Permissions</th><th><span class="sr-only">Actions</span></th></tr></thead>
            <tbody>
              <tr v-for="r in roles" :key="r.id">
                <td class="wrap"><strong>{{ r.name }}</strong><br v-if="r.description"><span class="muted small">{{ r.description }}</span></td>
                <td>{{ resourceLabel[r.resource] }}</td>
                <td><ul class="perms"><li v-for="p in summary(r)" :key="p" class="pill">{{ p }}</li></ul></td>
                <td class="num actions">
                  <button v-if="canManage" type="button" class="secondary tiny" :disabled="busy" :aria-label="`Edit ${r.name}`" @click="edit(r)">Edit</button>
                  <button v-if="canManage" type="button" class="secondary tiny" :disabled="busy" :aria-label="`Delete ${r.name}`" @click="deleting = r">Delete</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else-if="data" class="empty"><strong>No custom roles yet</strong>The built-in roles (read, write, full, owner) cover most needs; make one when a team needs a different mix.</p>
        <p v-else-if="!loadError" class="muted small" role="status">Loading roles…</p>
      </section>

      <section v-if="canManage" class="panel">
        <div class="panel__header">
          <h2>{{ editing ? `Edit ${editing.name}` : 'New role' }}</h2>
          <button v-if="editing" type="button" class="secondary tiny" @click="reset">Cancel</button>
        </div>
        <p v-if="!editing && data && roles.length >= data.limit" class="muted small">This organisation has {{ data.limit }} roles, the most it can have.</p>
        <form class="stack--tight" @submit.prevent="save">
          <div class="row">
            <div class="field grow"><label for="rname">Name</label><input id="rname" v-model="form.name" required maxlength="64" placeholder="e.g. Security triager" autocomplete="off"></div>
            <div class="field"><label for="rres">Applies to</label>
              <select id="rres" v-model="form.resource" :disabled="!!editing"><option value="repository">Repositories</option><option value="organisation">The organisation</option></select>
            </div>
          </div>
          <div class="field"><label for="rdesc">Description</label><input id="rdesc" v-model="form.description" maxlength="500" placeholder="What the role is for" autocomplete="off"></div>
          <PermissionPicker v-model="form.permissions" :catalog="catalog" :resource="form.resource" id-prefix="role" :disabled="busy" />
          <p v-if="form.resource === 'repository'" class="muted small">Repository roles always include reading the code.</p>
          <div><button type="submit" class="accent" :disabled="busy || !form.permissions.length">{{ editing ? 'Save role' : 'Create role' }}</button></div>
        </form>
      </section>

      <section class="panel">
        <h2>Organisation roles given</h2>
        <p class="muted small">Each person or team holds at most one custom organisation role; giving another replaces it.</p>
        <form v-if="canManage && orgRoles.length" class="row" @submit.prevent="assign">
          <div class="field"><label for="gkind">Give to</label><select id="gkind" v-model="kind"><option value="user">A person</option><option value="team">A team</option></select></div>
          <div class="field grow"><label for="gwho">{{ kind === 'user' ? 'Email' : 'Team' }}</label><input id="gwho" v-model="who" :type="kind === 'user' ? 'email' : 'text'" required :placeholder="kind === 'user' ? 'name@company.com' : 'e.g. platform'"></div>
          <div class="field"><label for="grole">Role</label><select id="grole" v-model="grantRole" required><option value="" disabled>Pick a role</option><option v-for="r in orgRoles" :key="r.id" :value="r.id">{{ r.name }}</option></select></div>
          <button type="submit" class="align-end" :disabled="busy">Give role</button>
        </form>
        <div v-if="grantsData?.grants.length" class="table-wrap">
          <table>
            <thead><tr><th>Who</th><th>Role</th><th><span class="sr-only">Actions</span></th></tr></thead>
            <tbody>
              <tr v-for="g in grantsData.grants" :key="g.subject_type + g.subject_id">
                <td class="wrap"><span class="badge">{{ g.subject_type === 'team' ? 'team' : 'person' }}</span> {{ g.label }}</td>
                <td>{{ g.role.name }}</td>
                <td class="num"><button v-if="canManage" type="button" class="secondary tiny" :disabled="busy" :aria-label="`Take ${g.role.name} from ${g.label}`" @click="revoke(g)">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="muted small">No organisation roles given.</p>
      </section>
    </template>
    <ConfirmDialog :open="!!deleting" title="Delete this role?" confirm-label="Delete role" danger :busy="busy" @confirm="remove" @cancel="deleting = null">
      <p>“{{ deleting?.name }}” can be deleted only when nobody holds it any more. Remove its grants first.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
.wrap { overflow-wrap: anywhere; }
.perms { display: flex; flex-wrap: wrap; gap: 4px; list-style: none; margin: 0; padding: 0; }
.actions { white-space: nowrap; }
.actions button + button { margin-left: 6px; }
</style>
