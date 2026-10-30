<script setup lang="ts">
import type { IdentityProvider, Role, Team } from '~/utils/types'

// The organisation's OpenID Connect provider (docs/plans/SSO_PLAN.md). Owners and
// admins manage it; the API checks discovery before saving and never returns
// the client secret.
const session = useSession()
const org = computed(() => session.current.value?.org)
const canManage = computed(() => session.can('admin'))
const { data, refresh, error: loadError } = useApiGet<{ provider: IdentityProvider | null; redirect_uri: string }>(() => (org.value ? `accounts/orgs/${org.value.slug}/sso` : 'accounts/orgs/-/sso'), { immediate: canManage.value })
const { data: teams } = useApiGet<{ teams: Team[] }>(() => (org.value ? `accounts/orgs/${org.value.slug}/teams` : 'accounts/orgs/-/teams'), { immediate: canManage.value })

const ROLES: Role[] = ['guest', 'viewer', 'member', 'admin', 'owner']
interface Mapping { group: string; value: string }
const form = reactive({ name: '', issuer: '', client_id: '', client_secret: '', enabled: true, groups_claim: 'groups', default_role: '' as Role | '', scopes: '' })
const roles = ref<Mapping[]>([])
const teamMaps = ref<Mapping[]>([])
watch(() => data.value?.provider, (p) => {
  Object.assign(form, { name: p?.name ?? '', issuer: p?.issuer ?? '', client_id: p?.client_id ?? '', client_secret: '', enabled: p?.enabled ?? true,
    groups_claim: p?.groups_claim || 'groups', default_role: p?.default_role ?? '', scopes: (p?.scopes ?? []).join(' ') })
  roles.value = Object.entries(p?.role_groups ?? {}).map(([group, value]) => ({ group, value }))
  teamMaps.value = Object.entries(p?.team_groups ?? {}).map(([group, value]) => ({ group, value }))
}, { immediate: true })

const signInLink = computed(() => (org.value && import.meta.client ? `${location.origin}/login?sso=${encodeURIComponent(org.value.slug)}` : ''))
const busy = ref(false)
const error = ref('')
const saved = ref('')
function toMap(list: Mapping[]) {
  return Object.fromEntries(list.filter((m) => m.group.trim() && m.value).map((m) => [m.group.trim(), m.value]))
}
async function save() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  saved.value = ''
  try {
    await apiPut(`accounts/orgs/${org.value!.slug}/sso`, {
        name: form.name.trim(), issuer: form.issuer.trim(), client_id: form.client_id.trim(), client_secret: form.client_secret,
        enabled: form.enabled, groups_claim: form.groups_claim.trim(), default_role: form.default_role || undefined,
        scopes: form.scopes.split(/\s+/).filter(Boolean), role_groups: toMap(roles.value), team_groups: toMap(teamMaps.value),
    })
    saved.value = 'Saved. The provider answered OpenID discovery.'
    await refresh()
  } catch (e) {
    error.value = plainError(e)
  } finally {
    busy.value = false
  }
}
const confirmRemove = ref(false)
async function remove() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  saved.value = ''
  try {
    await apiDelete(`accounts/orgs/${org.value!.slug}/sso`)
    saved.value = 'Single sign-on is disconnected.'
    await refresh()
  } catch (e) {
    error.value = plainError(e)
  } finally {
    busy.value = false
    confirmRemove.value = false
  }
}
watch(() => org.value?.slug, () => { error.value = ''; saved.value = '' })
// Which value was just copied, so its button can say so for a moment.
const copied = ref('')
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = text
    setTimeout(() => { if (copied.value === text) copied.value = '' }, 2000)
  } catch {
    copied.value = ''
  }
}
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <p v-else-if="!canManage" class="notice">Only owners and admins of {{ org.name }} can set up single sign-on.</p>
    <template v-else>
      <section class="panel">
        <h2>Single sign-on for {{ org.name }}</h2>
        <p class="muted small">Connect your organisation's OpenID Connect provider (Entra ID, Okta, Keycloak, Google Workspace, Authentik and others). People sign in through it, get an account the first time, and its groups decide their role and teams here. Members you added by hand keep the role you gave them, and the last owner is never removed. Passwords keep working.</p>
        <p v-if="loadError" class="notice notice--danger" role="alert">{{ plainError(loadError) }}</p>
        <dl v-if="data" class="facts">
          <dt>Redirect URI</dt>
          <dd><code>{{ data.redirect_uri }}</code> <button class="secondary tiny" type="button" aria-label="Copy the redirect URI" @click="copy(data.redirect_uri)">{{ copied === data.redirect_uri ? 'Copied' : 'Copy' }}</button><span class="hint">Register this with your provider as the sign-in redirect.</span></dd>
          <template v-if="data.provider">
            <dt>Sign-in link</dt>
            <dd><code>{{ signInLink }}</code> <button class="secondary tiny" type="button" aria-label="Copy the sign-in link" @click="copy(signInLink)">{{ copied === signInLink ? 'Copied' : 'Copy' }}</button><span class="hint">Send this to your people; it opens the sign-in page with your organisation filled in.</span></dd>
            <dt>Status</dt>
            <dd><span class="badge" :class="data.provider.enabled ? 'badge--green' : 'badge--amber'">{{ data.provider.enabled ? 'on' : 'off' }}</span> <span class="muted small">updated {{ relTime(data.provider.updated_at) }}</span></dd>
          </template>
        </dl>
      </section>

      <section class="panel">
        <h2>{{ data?.provider ? 'Provider' : 'Connect a provider' }}</h2>
        <form class="stack--tight" @submit.prevent="save">
          <div class="field"><label for="idp-name">Name</label><input id="idp-name" v-model="form.name" required maxlength="80" placeholder="Corp Entra ID"></div>
          <div class="field"><label for="idp-issuer">Issuer URL</label><input id="idp-issuer" v-model="form.issuer" type="url" required placeholder="https://login.example.com/realms/corp"><span class="hint">https only. CodeSynapse reads <code>/.well-known/openid-configuration</code> from it before saving.</span></div>
          <div class="row">
            <div class="field grow"><label for="idp-client">Client ID</label><input id="idp-client" v-model="form.client_id" required autocomplete="off"></div>
            <div class="field grow"><label for="idp-secret">Client secret</label><input id="idp-secret" v-model="form.client_secret" type="password" autocomplete="new-password" :required="!data?.provider" :placeholder="data?.provider ? `unchanged (${data.provider.secret_preview})` : ''"><span class="hint">Stored encrypted; only a preview is shown again.</span></div>
          </div>
          <div class="row">
            <div class="field"><label for="idp-claim">Groups claim</label><input id="idp-claim" v-model="form.groups_claim" placeholder="groups"></div>
            <div class="field"><label for="idp-default">Everyone else</label>
              <select id="idp-default" v-model="form.default_role">
                <option value="">No access (mapped groups only)</option>
                <option v-for="r in ROLES.filter((r) => r !== 'owner')" :key="r" :value="r">{{ r }}</option>
              </select>
            </div>
            <div class="field grow"><label for="idp-scopes">Extra scopes</label><input id="idp-scopes" v-model="form.scopes" placeholder="groups offline_access"><span class="hint">Besides openid, email and profile. Space-separated.</span></div>
          </div>

          <h3>Groups → role</h3>
          <div v-for="(m, i) in roles" :key="'r' + i" class="row map">
            <input v-model="m.group" placeholder="group name or ID" aria-label="Group">
            <select v-model="m.value" aria-label="Role"><option v-for="r in ROLES" :key="r" :value="r">{{ r }}</option></select>
            <button class="secondary tiny" type="button" :aria-label="`Remove the mapping for ${m.group || 'this group'}`" @click="roles.splice(i, 1)">Remove</button>
          </div>
          <div><button class="secondary tiny" type="button" @click="roles.push({ group: '', value: 'member' })">Add a role mapping</button></div>

          <h3>Groups → team</h3>
          <p v-if="!teams?.teams.length" class="muted small">No teams yet. Create them under <NuxtLink to="/settings/teams">Teams</NuxtLink> to map groups to them.</p>
          <template v-else>
            <div v-for="(m, i) in teamMaps" :key="'t' + i" class="row map">
              <input v-model="m.group" placeholder="group name or ID" aria-label="Group">
              <select v-model="m.value" aria-label="Team"><option v-for="t in teams.teams" :key="t.id" :value="t.id">{{ t.name }}</option></select>
              <button class="secondary tiny" type="button" :aria-label="`Remove the mapping for ${m.group || 'this group'}`" @click="teamMaps.splice(i, 1)">Remove</button>
            </div>
            <div><button class="secondary tiny" type="button" @click="teamMaps.push({ group: '', value: teams.teams[0]!.id })">Add a team mapping</button></div>
          </template>

          <label class="check"><input v-model="form.enabled" type="checkbox"> Allow sign-in through this provider</label>
          <p v-if="saved" class="notice notice--ok" role="status">{{ saved }}</p>
          <p v-if="error" class="notice notice--danger" role="alert">{{ error }}</p>
          <div class="row">
            <button type="submit" :disabled="busy">{{ busy ? 'Checking the provider…' : 'Save' }}</button>
            <button v-if="data?.provider" class="danger" type="button" :disabled="busy" @click="confirmRemove = true">Disconnect</button>
          </div>
        </form>
      </section>
      <ConfirmDialog :open="confirmRemove" title="Disconnect single sign-on?" confirm-label="Disconnect" danger :busy="busy" @confirm="remove" @cancel="confirmRemove = false">
        Nobody can sign in through {{ data?.provider?.name }} until it is connected again. Accounts, memberships and linked identities stay; people with a password can still sign in.
      </ConfirmDialog>
    </template>
  </div>
</template>

<style scoped>
.facts { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 8px 16px; margin: 0; }
.facts dt { color: var(--muted); }
.facts dd code { overflow-wrap: anywhere; min-width: 0; }
.facts dd { min-width: 0; margin: 0; display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.facts .hint { flex-basis: 100%; }
.grow { flex: 1 1 220px; }
.map input { flex: 1 1 200px; }
@media (max-width: 520px) { .facts { grid-template-columns: minmax(0, 1fr); } }
.check { display: flex; align-items: center; gap: 8px; }
</style>
