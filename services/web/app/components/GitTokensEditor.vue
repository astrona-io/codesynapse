<script setup lang="ts">
import type { UserGitToken } from '~/utils/types'

// Lists, adds and removes Git tokens under one API path: an organisation's
// (accounts/orgs/<slug>/git-tokens) or your own (me/git-tokens). The API
// decides who may change them; canManage only hides the controls.
const props = defineProps<{ path: string; title: string; canManage: boolean; removeNote: string; emptyNote: string }>()

const { data, pending, error: loadError, refresh } = useApiGet<{ tokens: Pick<UserGitToken, 'host' | 'preview' | 'updated_at'>[]; enabled: boolean }>(() => props.path)
const host = ref('github.com')
const token = ref('')
const error = ref('')
const restored = ref<string[]>([])
async function save() {
  error.value = ''
  restored.value = []
  try {
    const res = await $fetch<{ restored: string[] }>(apiPath(`${props.path}/${encodeURIComponent(host.value.trim().toLowerCase())}`), { method: 'PUT', body: { token: token.value } })
    restored.value = res.restored ?? []
    token.value = ''
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
const removing = ref<string | null>(null)
async function remove() {
  const h = removing.value
  removing.value = null
  if (!h) return
  error.value = ''
  try {
    await $fetch(apiPath(`${props.path}/${encodeURIComponent(h)}`), { method: 'DELETE' })
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
</script>

<template>
  <section class="panel">
    <h2>{{ title }}</h2>
    <slot />
    <p v-if="data && !data.enabled" class="notice notice--warn">Git tokens are switched off on this installation: the API needs <code>CODESYNAPSE_SECRET_KEY</code>. See <NuxtLink to="/help#git-tokens">Help</NuxtLink>.</p>
    <DataState :pending="pending" :error="loadError" :empty="!data?.tokens.length" title="No tokens" :text="emptyNote">
    <div class="table-wrap">
      <table>
        <thead><tr><th>Host</th><th>Token</th><th>Updated</th><th><span class="sr-only">Actions</span></th></tr></thead>
        <tbody>
          <tr v-for="t in data?.tokens ?? []" :key="t.host">
            <td class="mono break">{{ t.host }}</td>
            <td class="mono break">{{ t.preview }}</td>
            <td>{{ relTime(t.updated_at) }}</td>
            <td class="num"><button v-if="canManage" type="button" class="secondary tiny" :aria-label="`Remove the token for ${t.host}`" @click="removing = t.host">Remove</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    </DataState>
  </section>
  <section v-if="canManage && data?.enabled" class="panel">
    <h2>Add or replace a token</h2>
    <form class="stack--tight" @submit.prevent="save">
      <div class="row">
        <div class="field"><label :for="`${path}-host`">Host</label><input :id="`${path}-host`" v-model="host" required placeholder="github.com" class="host"></div>
        <div class="field grow"><label :for="`${path}-tok`">Token</label><input :id="`${path}-tok`" v-model="token" type="password" autocomplete="off" required minlength="8"></div>
      </div>
      <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
      <p v-if="restored.length" class="notice notice--ok" role="status">Access restored to {{ restored.join(', ') }}.</p>
      <div><button type="submit">Save token</button></div>
    </form>
  </section>
  <ConfirmDialog :open="!!removing" title="Remove this token?" confirm-label="Remove token" danger @confirm="remove" @cancel="removing = null">
    <p>Private repositories on {{ removing }} {{ removeNote }}</p>
  </ConfirmDialog>
</template>

<style scoped>
.grow { flex: 1; min-width: 240px; }
.host { width: min(220px, 100%); }
</style>
