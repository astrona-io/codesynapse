<script setup lang="ts">
import type { AdminOrg, SetupInput } from '~/utils/types'

// The root account's one organisation task: hand the installation to a
// standard owner. The API does the work in one transaction and ends this
// session.
definePageMeta({ layout: 'auth' })
useHead({ title: 'Finish setup · CodeSynapse' })
const session = useSession()
// An installation that predates root separation may already have
// organisations; its owner can take over one of them.
const { data: existing } = useApiGet<{ orgs: AdminOrg[] }>('admin/orgs')
const orgs = computed(() => existing.value?.orgs ?? [])
const useExisting = ref(false)
const orgName = ref('')
const orgId = ref('')
const name = ref('')
const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)
async function submit() {
  busy.value = true
  error.value = ''
  const body: SetupInput = { email: email.value, name: name.value, password: password.value }
  if (useExisting.value) body.org_id = orgId.value
  else body.org_name = orgName.value
  try {
    await apiPost('auth/setup', body)
    session.me.value = null
    await navigateTo('/setup/done')
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="stack--tight">
    <h1>Finish setup</h1>
    <p class="muted">You are signed in as the root account. It administers the installation (settings, health, the audit log) but never works in an organisation. Create the first organisation and the account that owns it; you then sign in with that account for organisation work.</p>
  </div>
  <form class="stack--tight" @submit.prevent="submit">
    <fieldset v-if="orgs.length" class="stack--tight">
      <legend class="label">Organisation</legend>
      <label class="check"><input v-model="useExisting" type="radio" :value="false"> Create a new organisation</label>
      <label class="check"><input v-model="useExisting" type="radio" :value="true"> Hand over an existing organisation</label>
    </fieldset>
    <div v-if="useExisting" class="field">
      <label for="org">Existing organisation</label>
      <select id="org" v-model="orgId" required>
        <option value="" disabled>Choose…</option>
        <option v-for="o in orgs" :key="o.id" :value="o.id">{{ o.name }}</option>
      </select>
    </div>
    <div v-else class="field"><label for="org-name">Organisation name</label><input id="org-name" v-model="orgName" required autofocus></div>
    <div class="field"><label for="name">Owner's name</label><input id="name" v-model="name" autocomplete="off" required></div>
    <div class="field">
      <label for="email">Owner's email</label>
      <input id="email" v-model="email" type="email" autocomplete="off" required>
      <span class="hint">A different person or address than the root account.</span>
    </div>
    <div class="field">
      <label for="password">Owner's password</label>
      <input id="password" v-model="password" type="password" autocomplete="new-password" minlength="12" required>
      <span class="hint">At least 12 characters. The owner signs in with it next.</span>
    </div>
    <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
    <button type="submit" :disabled="busy">{{ busy ? 'Setting up…' : 'Create the organisation and its owner' }}</button>
  </form>
  <p class="muted small"><button class="link" type="button" @click="session.logout()">Sign out</button></p>
</template>

<style scoped>
fieldset { border: 0; padding: 0; margin: 0; }
.check { display: flex; align-items: center; gap: 8px; color: var(--ink); }
</style>
