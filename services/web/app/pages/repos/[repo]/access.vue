<script setup lang="ts">
import type { RepoGrant, Role } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Access · ${repo.value}` })
const session = useSession()
const org = computed(() => session.current.value?.org)
const path = computed(() => `accounts/orgs/${org.value?.slug ?? '-'}/repos/${encodeURIComponent(repo.value)}/access`)
const { data, refresh, error: loadError } = useApiGet<{ grants: RepoGrant[]; default_repo_access: 'NONE' | 'READ_ONLY'; effective?: { role: Role; via: string } }>(() => path.value)
const canManage = computed(() => data.value?.effective?.role === 'admin')
const internal = computed(() => (data.value?.grants ?? []).filter((g) => !g.external))
const external = computed(() => (data.value?.grants ?? []).filter((g) => g.external))
const roleLabel: Record<string, string> = { none: 'No access: overrides all inherited access', viewer: 'Read: navigation and recall', member: 'Write: also writes memory and reviews', admin: 'Admin: also syncs and manages access' }
const kind = ref<'user' | 'team'>('user')
const who = ref('')
const role = ref<Role | 'none'>('viewer')
watch(kind, () => { if (kind.value === 'team' && role.value === 'none') role.value = 'viewer' })
const error = ref('')
const ok = ref('')
watch(repo, () => { error.value = ''; ok.value = ''; who.value = '' })
async function put(body: Record<string, string>) {
  error.value = ''
  ok.value = ''
  try {
    const g = await apiPut<RepoGrant>(path.value, body)
    ok.value = g.role === 'none' ? `${g.label} has no access to this repository.` : `Saved ${g.role === 'viewer' ? 'read' : g.role === 'member' ? 'write' : 'admin'} access for ${g.label}.`
    await refresh()
    return true
  } catch (e) {
    error.value = apiErrorMessage(e)
    return false
  }
}
async function grant() {
  const body: Record<string, string> = { role: role.value }
  body[kind.value] = kind.value === 'team' && !who.value.includes('/') ? `${org.value!.slug}/${who.value}` : who.value
  if (await put(body)) who.value = ''
}
async function revoke(g: RepoGrant) {
  error.value = ''
  try {
    await apiDelete(`${path.value}?subject=${encodeURIComponent(`${g.subject_type}:${g.subject_id}`)}`)
    await refresh()
  } catch (err) {
    error.value = apiErrorMessage(err)
  }
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Access" :title="`Who can use ${repo}`" lede="A direct user grant overrides team access. Otherwise the strongest team grant applies, then the organisation default. Every person must belong to the owning organisation." :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Access' }]" />
    <p v-if="!org" class="notice">Choose the organisation that owns this repository in the sidebar.</p>
    <template v-else>
      <p v-if="loadError" class="notice notice--danger" role="alert">{{ apiErrorMessage(loadError) }}</p>
      <p v-else-if="!data" class="muted" role="status">Loading access…</p>
      <section v-else class="panel">
        <h2>In {{ org.name }}</h2>
        <p class="muted small">Organisation default: {{ data.default_repo_access === 'READ_ONLY' ? 'read only' : 'no access' }}. Removing a direct grant restores inheritance.</p>
        <p v-if="data?.effective" class="muted small">Your permission: {{ roleLabel[data.effective.role] }}. From: {{ data.effective.via }}.</p>
        <form v-if="canManage" class="row" @submit.prevent="grant">
          <div class="field"><label for="kind">Give access to</label><select id="kind" v-model="kind"><option value="user">A person</option><option value="team">A team</option></select></div>
          <div class="field grow"><label for="who">{{ kind === 'user' ? 'Email' : 'Team' }}</label><input id="who" v-model="who" :type="kind === 'user' ? 'email' : 'text'" required :placeholder="kind === 'user' ? 'name@company.com' : 'e.g. contractors'"></div>
          <div class="field"><label for="role">Role</label><select id="role" v-model="role"><option value="viewer">Read</option><option value="member">Write</option><option value="admin">Admin</option><option v-if="kind === 'user'" value="none">No access (override)</option></select></div>
          <button type="submit" class="align-end">Give access</button>
        </form>
        <div v-if="internal.length" class="table-wrap">
          <table>
            <thead><tr><th>Who</th><th>Role</th><th><span class="sr-only">Actions</span></th></tr></thead>
            <tbody>
              <tr v-for="g in internal" :key="g.subject_type + g.subject_id">
                <td><span class="badge">{{ g.subject_type === 'team' ? 'team' : 'person' }}</span> {{ g.label }}</td>
                <td>{{ roleLabel[g.role] }}</td>
                <td class="num"><button v-if="canManage" class="secondary tiny" :aria-label="`Remove access for ${g.label}`" @click="revoke(g)">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="muted small">No direct grants.</p>
      </section>
      <section v-if="data && external.length" class="panel">
        <h2>Inactive external grants</h2>
        <p class="muted small">External grants do not provide access. Add a recipient as a guest of this organisation before giving them a repository grant.</p>
        <div v-if="external.length" class="table-wrap">
          <table>
            <thead><tr><th>Shared with</th><th>Since</th><th><span class="sr-only">Actions</span></th></tr></thead>
            <tbody>
              <tr v-for="g in external" :key="g.subject_type + g.subject_id">
                <td>{{ g.label }}</td>
                <td>{{ relTime(g.created_at) }}</td>
                <td class="num"><button v-if="canManage" class="secondary tiny" :aria-label="`Stop sharing with ${g.label}`" @click="revoke(g)">Stop sharing</button></td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
      <p v-if="ok" class="notice notice--ok" role="status">{{ ok }}</p>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    </template>
  </div>
</template>

<style scoped>
.grow { flex: 1 1 220px; min-width: 0; }
.align-end { align-self: flex-end; }
</style>
