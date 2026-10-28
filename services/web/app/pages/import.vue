<script setup lang="ts">
// Import with accounts: list an organisation's repositories (private ones too
// when the selected organisation has a Git token for the host), pick
// some, and add each one to the selected organisation the same way a single
// URL is added (POST /repos), so access checks and grants are the API's.
interface Candidate { name: string; repo: string; url: string; default_branch?: string; description?: string; archived?: boolean; fork?: boolean; private?: boolean; permission?: string; pushed_at?: string; added: boolean }
interface Listing { provider: string; host: string; org: string; repos: Candidate[]; truncated: boolean; authenticated?: boolean; sso_restricted?: boolean }

const PROVIDERS = [{ v: '', label: 'Detect from host' }, { v: 'github', label: 'GitHub' }, { v: 'forgejo', label: 'Forgejo / Gitea / Codeberg' }, { v: 'gitlab', label: 'GitLab' }]
const form = reactive({ provider: '', host: '', org: '', include_archived: false, include_forks: false })
const listing = ref<Listing>()
const loading = ref(false)
const error = ref('')
const picked = reactive<Record<string, boolean>>({})
const outcome = reactive<Record<string, { ok: boolean; text: string }>>({})
const filter = ref('')
const session = useSession()
const { space, canOrg, canMe, body: spaceBody } = useRepoSpace()

async function list() {
  error.value = ''
  loading.value = true
  try {
    listing.value = await apiPost<Listing>('imports/list', { ...form, host: form.host.trim(), org: form.org.trim() })
    for (const k of Object.keys(picked)) delete picked[k]
    for (const k of Object.keys(outcome)) delete outcome[k]
  } catch (e) {
    listing.value = undefined
    error.value = apiErrorMessage(e)
  } finally {
    loading.value = false
  }
}

const shown = computed(() => (listing.value?.repos ?? []).filter((r) => !filter.value || r.name.toLowerCase().includes(filter.value.toLowerCase())))
const open = computed(() => shown.value.filter((r) => !r.added && !outcome[r.repo]?.ok))
const chosen = computed(() => (listing.value?.repos ?? []).filter((r) => picked[r.repo] && !r.added && !outcome[r.repo]?.ok))
const allPicked = computed(() => open.value.length > 0 && open.value.every((r) => picked[r.repo]))
function pickAll(on: boolean) {
  for (const r of open.value) picked[r.repo] = on
}

const adding = ref(false)
// One at a time: the API queues clones anyway, and each answer says whether
// the repository was cloned or linked to an existing graph.
async function addChosen() {
  adding.value = true
  try {
    for (const r of chosen.value) {
      try {
        const res = await apiPost<{ shared?: boolean }>('repos', { url: r.url, name: r.repo, ...spaceBody() })
        outcome[r.repo] = { ok: true, text: res?.shared ? 'linked' : 'queued' }
        picked[r.repo] = false
      } catch (e) {
        outcome[r.repo] = { ok: false, text: apiErrorMessage(e) }
      }
    }
  } finally {
    adding.value = false
  }
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Workspace" title="Import repositories" lede="List the public repositories of an organisation or user on GitHub, Forgejo, Gitea or GitLab, and add the ones you pick to your organisation. Each one is cloned and indexed, or linked if it is already indexed." />

    <section class="panel">
      <form class="add" @submit.prevent="list">
        <label>Provider
          <select v-model="form.provider">
            <option v-for="p in PROVIDERS" :key="p.v" :value="p.v">{{ p.label }}</option>
          </select>
        </label>
        <label>Host <span class="muted small">(empty = github.com)</span>
          <input v-model="form.host" placeholder="git.example.com" autocomplete="off">
        </label>
        <label class="grow">Organisation or user
          <input v-model="form.org" required placeholder="astrona-io  ·  or https://codeberg.org/team/*" autocomplete="off">
        </label>
        <label class="check"><input v-model="form.include_archived" type="checkbox"> Include archived</label>
        <label class="check"><input v-model="form.include_forks" type="checkbox"> Include forks</label>
        <div class="row">
          <button type="submit" :disabled="!form.org.trim() || loading">{{ loading ? 'Listing…' : 'List repositories' }}</button>
          <span class="muted small">Private repositories are listed when your organisation has a Git token for the host in <NuxtLink to="/settings">Settings</NuxtLink>; otherwise only public ones are.</span>
        </div>
      </form>
      <p v-if="error" class="error">{{ error }}</p>
    </section>

    <section v-if="listing" class="panel org">
      <div class="org-head">
        <div>
          <p class="eyebrow">{{ listing.host }}</p>
          <h2 class="mono">{{ listing.org }}/*</h2>
          <p class="muted small">{{ plural(listing.repos.length, 'repository', 'repositories') }} · {{ listing.repos.filter((r) => r.added).length }} already in your organisation<template v-if="listing.truncated"> · only the first {{ listing.repos.length }} are shown</template><template v-if="listing.authenticated"> · listed with your organisation's token</template></p>
          <p v-if="listing.sso_restricted" class="muted small">GitHub left out repositories the token is not authorised for under the organisation's SAML single sign-on. Authorise the token for the organisation in GitHub's token settings.</p>
        </div>
        <div class="row tight-actions">
          <button :disabled="!chosen.length || adding" @click="addChosen">{{ adding ? 'Adding…' : chosen.length ? `Add ${chosen.length} selected` : 'Add selected' }}</button>
        </div>
      </div>
      <fieldset v-if="session.accounts.value" class="space">
        <legend>Add to</legend>
        <label class="check"><input v-model="space" type="radio" value="org" :disabled="!canOrg"> The organisation<span v-if="!canOrg" class="muted small"> (owners and admins)</span></label>
        <label class="check"><input v-model="space" type="radio" value="me" :disabled="!canMe"> Only me<span v-if="!canMe" class="muted small"> (turned off by your organisation)</span></label>
      </fieldset>
      <div class="row opts">
        <input v-model="filter" class="find" placeholder="Filter repositories" aria-label="Filter repositories">
        <label class="check"><input type="checkbox" :checked="allPicked" :disabled="!open.length" @change="pickAll(($event.target as HTMLInputElement).checked)"> Select all shown</label>
      </div>
      <ul class="repos">
        <li v-for="r in shown" :key="r.repo" class="repo">
          <input v-if="!r.added && !outcome[r.repo]?.ok" v-model="picked[r.repo]" type="checkbox" :aria-label="`Select ${r.name}`">
          <NuxtLink v-if="r.added" :to="`/repos/${encodeURIComponent(r.repo)}`" class="mono name" :title="r.repo">{{ r.name }}</NuxtLink>
          <span v-else class="mono name">{{ r.name }}</span>
          <span v-if="r.archived" class="badge badge--amber">archived</span>
          <span v-if="r.fork" class="badge badge--violet">fork</span>
          <span v-if="r.private" class="badge badge--pink">private</span>
          <span v-if="r.permission" class="badge badge--violet" :title="`The organisation's token has ${r.permission} access`">{{ r.permission }}</span>
          <span v-if="r.added" class="badge badge--green">added</span>
          <span v-else-if="outcome[r.repo]?.ok" class="badge badge--cyan">{{ outcome[r.repo]!.text }}</span>
          <span class="muted small desc">{{ r.description }}</span>
          <p v-if="outcome[r.repo] && !outcome[r.repo]!.ok" class="error small full">{{ outcome[r.repo]!.text }}</p>
        </li>
      </ul>
      <div v-if="!listing.repos.length" class="empty"><strong>Nothing to import</strong>No repositories match. Try including archived repositories or forks.</div>
    </section>
  </div>
</template>

<style scoped>
.add { display: grid; gap: 10px; }
.add label { display: grid; gap: 4px; }
.check { display: flex !important; align-items: center; gap: 8px; }
.grow { flex: 1 1 220px; }
.space { display: flex; flex-wrap: wrap; gap: 6px 16px; border: 0; padding: 0; margin: 0; }
.space legend { font-weight: 600; margin-bottom: 4px; }
.org { display: grid; gap: 12px; }
.org-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; flex-wrap: wrap; }
.opts { gap: 18px; flex-wrap: wrap; align-items: center; }
.find { max-width: 320px; }
.repos { list-style: none; margin: 0; padding: 0; display: grid; }
.repo { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 8px 0; border-bottom: 1px dashed var(--line); }
.repo:last-child { border-bottom: 0; }
.name { font-weight: 600; }
.desc { flex: 1 1 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.full { flex-basis: 100%; }
.small { font-size: 12.5px; }
</style>
