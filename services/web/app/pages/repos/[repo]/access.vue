<script setup lang="ts">
import type { RepoGrant, Role } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Access · ${repo.value}` })
const session = useSession()
const org = computed(() => session.current.value?.org)
const path = computed(() => `accounts/orgs/${org.value?.slug ?? '-'}/repos/${encodeURIComponent(repo.value)}/access`)
const { data, refresh, error: loadError } = useApiGet<{ grants: RepoGrant[] }>(() => path.value)
const internal = computed(() => (data.value?.grants ?? []).filter((g) => !g.external))
const external = computed(() => (data.value?.grants ?? []).filter((g) => g.external))
const roleLabel: Record<string, string> = { viewer: 'Viewer: reads', member: 'Editor: also syncs, writes memory and reviews', admin: 'Admin: also manages access' }
const kind = ref<'user' | 'team'>('user')
const who = ref('')
const role = ref<Role>('viewer')
const shareEmail = ref('')
const error = ref('')
const ok = ref('')
async function put(body: Record<string, string>) {
  error.value = ''
  ok.value = ''
  try {
    const g = await $fetch<RepoGrant>(apiPath(path.value), { method: 'PUT', body })
    ok.value = g.external ? `Shared read-only with ${g.label}.` : `${g.label} can now ${g.role === 'viewer' ? 'read' : g.role === 'member' ? 'edit' : 'administer'} this repository.`
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
async function share() {
  if (await put({ user: shareEmail.value, role: 'viewer' })) shareEmail.value = ''
}
async function revoke(g: RepoGrant) {
  await $fetch(apiPath(`${path.value}?subject=${g.subject_type}:${g.subject_id}`), { method: 'DELETE' })
  await refresh()
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Access" :title="`Who can use ${repo}`" lede="Everyone in your organisation with viewer, member, admin or owner role reaches this repository through their role. Here you give access to more people: guests, teams, or someone in another organisation." :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Access' }]" />
    <p v-if="!org" class="notice">Choose the organisation that owns this repository in the sidebar.</p>
    <template v-else>
      <p v-if="loadError" class="notice notice--danger">{{ apiErrorMessage(loadError) }}</p>
      <section class="panel">
        <h2>In {{ org.name }}</h2>
        <form class="row" @submit.prevent="grant">
          <div class="field"><label for="kind">Give access to</label><select id="kind" v-model="kind"><option value="user">A person</option><option value="team">A team</option></select></div>
          <div class="field grow"><label for="who">{{ kind === 'user' ? 'Email' : 'Team' }}</label><input id="who" v-model="who" :type="kind === 'user' ? 'email' : 'text'" required :placeholder="kind === 'user' ? 'name@company.com' : 'e.g. contractors'"></div>
          <div class="field"><label for="role">Role</label><select id="role" v-model="role"><option value="viewer">Viewer</option><option value="member">Editor</option><option value="admin">Admin</option></select></div>
          <button type="submit" class="align-end">Give access</button>
        </form>
        <div v-if="internal.length" class="table-wrap">
          <table>
            <thead><tr><th>Who</th><th>Role</th><th /></tr></thead>
            <tbody>
              <tr v-for="g in internal" :key="g.subject_type + g.subject_id">
                <td><span class="badge">{{ g.subject_type === 'team' ? 'team' : 'person' }}</span> {{ g.label }}</td>
                <td>{{ roleLabel[g.role] }}</td>
                <td class="num"><button class="secondary tiny" @click="revoke(g)">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="muted small">No extra access given yet.</p>
      </section>
      <section class="panel">
        <h2>Shared outside {{ org.name }}</h2>
        <p class="muted small">Someone in another organisation, such as an auditor or a partner, gets read-only access: this repository's reports, reviews and agent memory, nothing else of yours. Only organisation admins can share outside.</p>
        <form class="row" @submit.prevent="share">
          <div class="field grow"><label for="share">Their email</label><input id="share" v-model="shareEmail" type="email" required placeholder="auditor@partner.com"></div>
          <button type="submit" class="align-end secondary">Share read-only</button>
        </form>
        <div v-if="external.length" class="table-wrap">
          <table>
            <thead><tr><th>Shared with</th><th>Since</th><th /></tr></thead>
            <tbody>
              <tr v-for="g in external" :key="g.subject_type + g.subject_id">
                <td>{{ g.label }}</td>
                <td>{{ relTime(g.created_at) }}</td>
                <td class="num"><button class="secondary tiny" @click="revoke(g)">Stop sharing</button></td>
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
.grow { flex: 1; min-width: 220px; }
.align-end { align-self: flex-end; }
</style>
