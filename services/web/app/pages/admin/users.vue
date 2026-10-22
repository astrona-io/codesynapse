<script setup lang="ts">
import type { AdminUser } from '~/utils/types'

const session = useSession()
const { data, error, refresh } = useApiGet<{ users: AdminUser[] }>('admin/users')
const q = ref('')
const shown = computed(() => (data.value?.users ?? []).filter((u) => !q.value || `${u.name} ${u.email}`.toLowerCase().includes(q.value.toLowerCase())))
type Act = { user: AdminUser; kind: 'suspend' | 'resume' | 'grant' | 'revoke' }
const pendingAct = ref<Act | null>(null)
const msg = ref('')
const TEXT = {
  suspend: { title: 'Suspend this account?', label: 'Suspend', body: 'They are signed out everywhere, cannot sign in, and their API keys stop working at the next request. Memberships, keys and history stay, so you can resume the account later.' },
  resume: { title: 'Resume this account?', label: 'Resume', body: 'They can sign in again and their API keys work again.' },
  grant: { title: 'Make this person a platform administrator?', label: 'Make administrator', body: 'They will see every organisation, every user and the audit log, and can suspend accounts.' },
  revoke: { title: 'Remove platform administrator rights?', label: 'Remove rights', body: 'They keep their own organisations, but no longer see the rest of the installation.' },
}
async function confirm() {
  const a = pendingAct.value
  pendingAct.value = null
  if (!a) return
  msg.value = ''
  try {
    if (a.kind === 'suspend' || a.kind === 'resume') await apiPost(`admin/users/${a.user.id}/${a.kind}`, {})
    else await $fetch(apiPath(`admin/users/${a.user.id}/platform-admin`), { method: 'PUT', body: { admin: a.kind === 'grant' } })
  } catch (e) {
    msg.value = apiErrorMessage(e)
  }
  await refresh()
}
const self = (u: AdminUser) => u.id === session.me.value?.user.id
</script>

<template>
  <section class="panel">
    <div class="panel__header">
      <h2>People <span class="muted">{{ data?.users.length ?? 0 }}</span></h2>
      <input v-model="q" class="search" placeholder="Filter by name or email" aria-label="Filter people">
    </div>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <p v-if="msg" class="notice notice--danger" role="alert">{{ msg }}</p>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Person</th><th>Organisations</th><th class="num">Active keys</th><th>Last agent use</th><th>Joined</th><th /></tr></thead>
        <tbody>
          <tr v-for="u in shown" :key="u.id" :class="{ suspended: u.disabled_at }">
            <td>
              <strong>{{ u.name }}</strong><span v-if="self(u)" class="muted"> (you)</span>
              <div class="muted small">{{ u.email }}</div>
              <span v-if="u.platform_admin" class="badge badge--orange">platform admin</span>
              <span v-if="u.disabled_at" class="badge badge--pink">suspended {{ relTime(u.disabled_at) }}</span>
            </td>
            <td class="small">{{ u.orgs.map((o) => `${o.name} (${o.role})`).join(', ') || '–' }}</td>
            <td class="num">{{ u.active_keys }}</td>
            <td>{{ u.last_key_use ? relTime(u.last_key_use) : 'never' }}</td>
            <td>{{ new Date(u.created_at).toLocaleDateString() }}</td>
            <td class="num nowrap">
              <template v-if="!self(u)">
                <button v-if="u.disabled_at" class="secondary tiny" @click="pendingAct = { user: u, kind: 'resume' }">Resume</button>
                <button v-else class="secondary tiny" @click="pendingAct = { user: u, kind: 'suspend' }">Suspend</button>
                <button v-if="u.platform_admin" class="secondary tiny" @click="pendingAct = { user: u, kind: 'revoke' }">Remove admin</button>
                <button v-else-if="!u.disabled_at" class="secondary tiny" @click="pendingAct = { user: u, kind: 'grant' }">Make admin</button>
              </template>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <ConfirmDialog :open="!!pendingAct" :title="pendingAct ? TEXT[pendingAct.kind].title : ''" :confirm-label="pendingAct ? TEXT[pendingAct.kind].label : ''" :danger="pendingAct?.kind === 'suspend' || pendingAct?.kind === 'grant'" @confirm="confirm" @cancel="pendingAct = null">
      <p><strong>{{ pendingAct?.user.name }}</strong> ({{ pendingAct?.user.email }})</p>
      <p>{{ pendingAct ? TEXT[pendingAct.kind].body : '' }}</p>
    </ConfirmDialog>
  </section>
</template>

<style scoped>
.search { width: 240px; min-height: 30px; padding: 4px 10px; }
.nowrap { white-space: nowrap; }
tr.suspended td { opacity: 0.7; }
td .badge { margin-top: 4px; }
</style>
