<script setup lang="ts">
import type { ApiKey } from '~/utils/types'

const session = useSession()
const { data, refresh, error: loadError } = useApiGet<{ keys: ApiKey[] }>('me/keys')
const name = ref('')
const ttl = ref(0)
const created = ref('')
const error = ref('')
const notice = ref('')
const creating = ref(false)
const orgName = (id: string) => session.memberships.value.find((m) => m.org.id === id)?.org.name ?? 'another organisation'
const active = computed(() => (data.value?.keys ?? []).filter((k) => !k.revoked_at))
// A new key is shown once, for the organisation it was made in: switching
// organisation hides it rather than leaving it beside another one's name.
watch(() => session.current.value?.org.slug, () => { created.value = ''; error.value = ''; notice.value = '' })
async function create() {
  if (creating.value) return
  creating.value = true
  error.value = ''
  notice.value = ''
  created.value = ''
  const org = session.current.value?.org.slug ?? session.memberships.value[0]?.org.slug
  try {
    const res = await apiPost<{ key: string }>('me/keys', { org, name: name.value, ttl_days: ttl.value })
    created.value = res.key
    name.value = ''
    await refresh()
  } catch (e) {
    error.value = plainError(e)
  } finally {
    creating.value = false
  }
}
const revoking = ref<ApiKey | null>(null)
const revokeBusy = ref(false)
async function revoke() {
  const k = revoking.value
  if (!k || revokeBusy.value) return
  revokeBusy.value = true
  error.value = ''
  notice.value = ''
  try {
    await apiDelete(`me/keys/${encodeURIComponent(k.id)}`)
    notice.value = `Revoked “${k.name}”.`
  } catch (e) {
    error.value = plainError(e)
  } finally {
    revokeBusy.value = false
    revoking.value = null
    await refresh()
  }
}
</script>

<template>
  <div class="stack">
    <section class="panel">
      <h2>New API key</h2>
      <p class="muted small">An API key lets an agent, a hook or the CLI act as you in <strong>{{ session.current.value?.org.name ?? 'your organisation' }}</strong>. It reaches only repositories you can access there; its memory, reviews and statistics stay there. Keys cannot register repositories or change access. The <NuxtLink to="/agents">Agents</NuxtLink> page walks you through connecting one.</p>
      <form class="row" @submit.prevent="create">
        <div class="field grow"><label for="kname">Name</label><input id="kname" v-model="name" placeholder="e.g. laptop, CI" required maxlength="80" autocomplete="off"></div>
        <div class="field"><label for="ttl">Expires</label>
          <select id="ttl" v-model.number="ttl"><option :value="0">Never</option><option :value="30">In 30 days</option><option :value="90">In 90 days</option><option :value="365">In a year</option></select>
        </div>
        <button type="submit" class="align-end" :disabled="creating">{{ creating ? 'Creating…' : 'Create key' }}</button>
      </form>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
      <p v-if="notice" class="notice notice--ok" role="status">{{ notice }}</p>
      <div v-if="created" class="notice notice--ok stack--tight" role="status">
        <strong>Copy the key now. It is shown only once.</strong>
        <CopyField :value="created" secret />
        <div><button type="button" class="secondary tiny" @click="created = ''">I have copied it</button></div>
      </div>
    </section>
    <section class="panel">
      <h2>Your keys</h2>
      <p v-if="loadError" class="notice notice--danger" role="alert">{{ plainError(loadError) }}</p>
      <div v-else-if="active.length" class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Key</th><th>Organisation</th><th>Last used</th><th>Expires</th><th><span class="sr-only">Actions</span></th></tr></thead>
          <tbody>
            <tr v-for="k in active" :key="k.id">
              <td>{{ k.name }}</td>
              <td class="mono">{{ k.prefix }}…</td>
              <td>{{ orgName(k.org_id) }}</td>
              <td>{{ k.last_used_at ? relTime(k.last_used_at) : 'never' }}</td>
              <td>{{ k.expires_at ? new Date(k.expires_at).toLocaleDateString() : 'never' }}</td>
              <td class="num"><button type="button" class="secondary tiny" :aria-label="`Revoke ${k.name}`" @click="revoking = k">Revoke</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else-if="data" class="empty"><strong>No keys yet</strong>Create one above to connect an agent.</p>
      <p v-else class="muted small" role="status">Loading your keys…</p>
    </section>
    <ConfirmDialog :open="!!revoking" title="Revoke this key?" confirm-label="Revoke key" danger :busy="revokeBusy" @confirm="revoke" @cancel="revoking = null">
      <p>Agents using “{{ revoking?.name }}” stop working at their next request. This cannot be undone.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 200px; }
.align-end { align-self: flex-end; }
</style>
