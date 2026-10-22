<script setup lang="ts">
import type { GitToken } from '~/utils/types'

const session = useSession()
const org = computed(() => session.current.value?.org)
const { data, refresh } = useApiGet<{ tokens: GitToken[]; enabled: boolean }>(() => (org.value ? `accounts/orgs/${org.value.slug}/git-tokens` : 'accounts/orgs/-/git-tokens'))
const canManage = computed(() => session.can('admin'))
const host = ref('github.com')
const token = ref('')
const error = ref('')
const restored = ref<string[]>([])
async function save() {
  error.value = ''
  restored.value = []
  try {
    const res = await $fetch<{ restored: string[] }>(apiPath(`accounts/orgs/${org.value!.slug}/git-tokens/${encodeURIComponent(host.value.trim().toLowerCase())}`), { method: 'PUT', body: { token: token.value } })
    restored.value = res.restored ?? []
    token.value = ''
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
const removing = ref<GitToken | null>(null)
async function remove() {
  const t = removing.value
  removing.value = null
  if (!t) return
  await $fetch(apiPath(`accounts/orgs/${org.value!.slug}/git-tokens/${encodeURIComponent(t.host)}`), { method: 'DELETE' })
  await refresh()
}
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <section class="panel">
        <h2>Git tokens for {{ org.name }}</h2>
        <p class="muted small">Public repositories need no token. For private ones, store a read-only token per Git host (a GitHub fine-grained token with Contents: read, a GitLab project or group token with read_repository). Tokens are encrypted, shown only as a preview, and used only to clone and sync your organisation's repositories. CodeSynapse checks at every sync that the token can still read each private repository; if it cannot, the repository is hidden from your organisation until a working token is saved.</p>
        <p v-if="data && !data.enabled" class="notice notice--warn">Git tokens are switched off on this installation: the API needs <code>CODESYNAPSE_SECRET_KEY</code>. See <NuxtLink to="/help#git-tokens">Help</NuxtLink>.</p>
        <div v-if="data?.tokens.length" class="table-wrap">
          <table>
            <thead><tr><th>Host</th><th>Token</th><th>Updated</th><th /></tr></thead>
            <tbody>
              <tr v-for="t in data.tokens" :key="t.host">
                <td class="mono">{{ t.host }}</td>
                <td class="mono">{{ t.preview }}</td>
                <td>{{ relTime(t.updated_at) }}</td>
                <td class="num"><button v-if="canManage" class="secondary tiny" @click="removing = t">Remove</button></td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="empty"><strong>No tokens</strong>Your organisation can add public repositories. Add a token to add private ones.</p>
      </section>
      <section v-if="canManage && data?.enabled" class="panel">
        <h2>Add or replace a token</h2>
        <form class="stack--tight" @submit.prevent="save">
          <div class="row">
            <div class="field"><label for="host">Host</label><input id="host" v-model="host" required placeholder="github.com" class="host"></div>
            <div class="field grow"><label for="tok">Token</label><input id="tok" v-model="token" type="password" autocomplete="off" required minlength="8"></div>
          </div>
          <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
          <p v-if="restored.length" class="notice notice--ok" role="status">Access restored to {{ restored.join(', ') }}.</p>
          <div><button type="submit">Save token</button></div>
        </form>
      </section>
      <p v-else-if="!canManage" class="muted small">Only admins and owners can add or remove tokens.</p>
    </template>
    <ConfirmDialog :open="!!removing" title="Remove this token?" confirm-label="Remove token" danger @confirm="remove" @cancel="removing = null">
      <p>Private repositories on {{ removing?.host }} become hidden from {{ org?.name }} at their next sync.</p>
    </ConfirmDialog>
  </div>
</template>

<style scoped>
.grow { flex: 1; min-width: 240px; }
.host { width: 220px; }
</style>
