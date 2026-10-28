<script setup lang="ts">
import type { Job } from '~/utils/types'

// Add repositories: one by URL, or the repositories of an organisation or
// user (listed with the organisation's token for the host when it has one,
// then picked). Both end in POST /repos, so
// access checks and grants stay the API's.
interface Candidate { name: string; repo: string; url: string; archived?: boolean; fork?: boolean; private?: boolean; permission?: string; description?: string; added: boolean }
interface Listing { provider: string; host: string; org: string; repos: Candidate[]; truncated: boolean; authenticated?: boolean; sso_restricted?: boolean }

const props = defineProps<{ open: boolean; cloneEnabled?: boolean; hostTokens?: string[]; busy?: boolean }>()
const emit = defineEmits<{ close: []; added: [] }>()
const session = useSession()

const PROVIDERS = [{ v: '', label: 'Detect from host' }, { v: 'github', label: 'GitHub' }, { v: 'forgejo', label: 'Forgejo / Gitea / Codeberg' }, { v: 'gitlab', label: 'GitLab' }]
const dialog = ref<HTMLDialogElement | null>(null)
const tab = ref<'single' | 'org'>('single')
watch(() => props.open, (open) => {
  if (open && !dialog.value?.open) dialog.value?.showModal()
  else if (!open && dialog.value?.open) dialog.value.close()
})

const reg = reactive({ url: '', branch: '', name: '' })
const regError = ref('')
const regOk = ref('')
const urlHost = computed(() => { try { return new URL(reg.url.trim()).hostname.toLowerCase() } catch { return '' } })
const hostHasToken = computed(() => !!urlHost.value && (props.hostTokens ?? []).includes(urlHost.value))
async function register() {
  regError.value = ''
  regOk.value = ''
  try {
    const job = await apiPost<Job & { shared?: boolean }>('repos', { url: reg.url.trim(), branch: reg.branch.trim(), name: reg.name.trim() })
    regOk.value = job.shared ? 'Already indexed: linked to your organisation.' : `Cloning ${job.repo}… it appears in the list when indexing finishes.`
    reg.url = ''; reg.branch = ''; reg.name = ''
    emit('added')
  } catch (e) {
    regError.value = apiErrorMessage(e)
  }
}

const form = reactive({ provider: '', host: '', org: '', include_archived: false, include_forks: false })
const listing = ref<Listing>()
const loading = ref(false)
const listError = ref('')
const picked = reactive<Record<string, boolean>>({})
const outcome = reactive<Record<string, { ok: boolean; text: string }>>({})
const filter = ref('')
async function list() {
  listError.value = ''
  loading.value = true
  try {
    listing.value = await apiPost<Listing>('imports/list', { ...form, host: form.host.trim(), org: form.org.trim() })
    for (const k of Object.keys(picked)) delete picked[k]
    for (const k of Object.keys(outcome)) delete outcome[k]
  } catch (e) {
    listing.value = undefined
    listError.value = apiErrorMessage(e)
  } finally {
    loading.value = false
  }
}
const shown = computed(() => (listing.value?.repos ?? []).filter((r) => !filter.value || r.name.toLowerCase().includes(filter.value.toLowerCase())))
const openRepos = computed(() => shown.value.filter((r) => !r.added && !outcome[r.repo]?.ok))
const chosen = computed(() => (listing.value?.repos ?? []).filter((r) => picked[r.repo] && !r.added && !outcome[r.repo]?.ok))
const allPicked = computed(() => openRepos.value.length > 0 && openRepos.value.every((r) => picked[r.repo]))
const pickAll = (on: boolean) => { for (const r of openRepos.value) picked[r.repo] = on }

// Watching the organisation adds the repositories created from now on; the
// ones listed today are the baseline, so unpicked ones stay out.
const autoAdd = ref(true)
const watchNote = ref('')
const adding = ref(false)
// One at a time: the API queues clones, and each answer says whether the
// repository was cloned or linked to an existing graph.
async function addChosen() {
  adding.value = true
  watchNote.value = ''
  try {
    for (const r of chosen.value) {
      try {
        const res = await apiPost<{ shared?: boolean }>('repos', { url: r.url, name: r.repo })
        outcome[r.repo] = { ok: true, text: res?.shared ? 'linked' : 'queued' }
        picked[r.repo] = false
      } catch (e) {
        outcome[r.repo] = { ok: false, text: apiErrorMessage(e) }
      }
    }
    if (autoAdd.value && listing.value) {
      try {
        await apiPost('watches', { provider: listing.value.provider, host: listing.value.host, org: listing.value.org, include_archived: form.include_archived, include_forks: form.include_forks })
        watchNote.value = `Watching ${listing.value.org}: new public repositories are added automatically.`
      } catch (e) {
        watchNote.value = `Could not watch ${listing.value.org}: ${apiErrorMessage(e)}`
      }
    }
  } finally {
    adding.value = false
    emit('added')
  }
}
</script>

<template>
  <Teleport to="#teleports">
    <dialog ref="dialog" class="add-dialog" @cancel.prevent="emit('close')" @close="emit('close')">
      <div class="body">
        <header class="head">
          <div>
            <p class="eyebrow">Add repositories</p>
            <h2>Scan one repository or a whole organisation</h2>
          </div>
          <button type="button" class="secondary tiny" aria-label="Close" @click="emit('close')">Close</button>
        </header>

        <div class="tabs" role="tablist">
          <button type="button" role="tab" :aria-selected="tab === 'single'" :class="{ on: tab === 'single' }" @click="tab = 'single'">Single repository</button>
          <button type="button" role="tab" :aria-selected="tab === 'org'" :class="{ on: tab === 'org' }" @click="tab = 'org'">Whole organisation</button>
        </div>

        <form v-if="tab === 'single'" class="form" @submit.prevent="register">
          <label>Repository URL
            <input v-model="reg.url" type="url" required placeholder="https://github.com/org/repo.git" autocomplete="off">
          </label>
          <div class="row">
            <label class="grow">Branch <span class="dim">(optional)</span><input v-model="reg.branch" placeholder="default branch"></label>
            <label class="grow">Name <span class="dim">(optional)</span><input v-model="reg.name" placeholder="owner/name from the URL"></label>
          </div>
          <div class="row">
            <button type="submit" :disabled="!reg.url || busy || cloneEnabled === false">Clone &amp; index</button>
            <span class="muted small">Shallow clone over HTTPS; nothing is pushed.</span>
          </div>
          <p v-if="regOk" class="ok">{{ regOk }}</p>
          <p v-if="regError" class="error">{{ regError }}</p>
          <p v-if="urlHost && !hostHasToken" class="muted small">No Git token is set for {{ urlHost }}: if this repository is private, cloning will fail.</p>
        </form>

        <div v-else-if="!session.accounts.value" class="form">
          <p class="muted">Without accounts, organisations are watched installation-wide and every repository is indexed and kept in sync.</p>
          <NuxtLink to="/orgs" class="button-link">Open organisation import</NuxtLink>
        </div>

        <div v-else class="form">
          <form class="form" @submit.prevent="list">
            <div class="row">
              <label class="grow">Provider
                <select v-model="form.provider"><option v-for="p in PROVIDERS" :key="p.v" :value="p.v">{{ p.label }}</option></select>
              </label>
              <label class="grow">Host <span class="dim">(empty = github.com)</span>
                <input v-model="form.host" placeholder="git.example.com" autocomplete="off">
              </label>
            </div>
            <label>Organisation or user
              <input v-model="form.org" required placeholder="astrona-io  ·  or https://codeberg.org/team/*" autocomplete="off">
            </label>
            <div class="row opts">
              <label class="check"><input v-model="form.include_archived" type="checkbox"> Include archived</label>
              <label class="check"><input v-model="form.include_forks" type="checkbox"> Include forks</label>
              <button type="submit" :disabled="!form.org.trim() || loading">{{ loading ? 'Listing…' : 'List repositories' }}</button>
            </div>
            <p class="muted small"><template v-if="listing?.authenticated">Listed with your organisation's token for {{ listing.host }}: private repositories it can read are included.</template><template v-else>Private repositories are listed when your organisation has a Git token for the host in Settings; otherwise only public ones are.</template></p>
            <p v-if="listing?.sso_restricted" class="muted small">GitHub left out repositories the token is not authorised for under the organisation's SAML single sign-on. Authorise the token for the organisation in GitHub's token settings.</p>
            <p v-if="listError" class="error">{{ listError }}</p>
          </form>

          <section v-if="listing" class="found">
            <div class="found-head">
              <p class="muted small"><strong class="mono">{{ listing.org }}/*</strong> · {{ plural(listing.repos.length, 'repository', 'repositories') }} · {{ listing.repos.filter((r) => r.added).length }} already added<template v-if="listing.truncated"> · only the first {{ listing.repos.length }} are shown</template></p>
              <button type="button" :disabled="(!chosen.length && !autoAdd) || adding" @click="addChosen">{{ adding ? 'Adding…' : chosen.length ? `Add ${chosen.length} selected` : autoAdd ? 'Watch for new repositories' : 'Add selected' }}</button>
            </div>
            <label class="check"><input v-model="autoAdd" type="checkbox"> Automatically add new repositories {{ listing.org }} creates later</label>
            <p v-if="watchNote" class="muted small">{{ watchNote }}</p>
            <div class="row opts">
              <input v-model="filter" class="find" placeholder="Filter repositories" aria-label="Filter repositories">
              <label class="check"><input type="checkbox" :checked="allPicked" :disabled="!openRepos.length" @change="pickAll(($event.target as HTMLInputElement).checked)"> Select all shown</label>
            </div>
            <ul class="repos">
              <li v-for="r in shown" :key="r.repo" class="repo">
                <input v-if="!r.added && !outcome[r.repo]?.ok" v-model="picked[r.repo]" type="checkbox" :aria-label="`Select ${r.name}`">
                <span class="mono name">{{ r.name }}</span>
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
      </div>
    </dialog>
  </Teleport>
</template>

<style scoped>
.add-dialog { border: 1px solid var(--line-strong); border-radius: var(--radius); background: var(--panel); color: var(--ink); padding: 0; width: min(720px, calc(100vw - 32px)); max-height: calc(100vh - 48px); box-shadow: var(--shadow-pop); }
.add-dialog::backdrop { background: rgba(15, 17, 21, 0.45); backdrop-filter: blur(4px); }
.body { display: grid; gap: 14px; padding: 22px 24px; }
.head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
.form { display: grid; gap: 10px; }
.form label { display: grid; gap: 4px; }
.grow { flex: 1 1 180px; }
.opts { gap: 18px; flex-wrap: wrap; align-items: center; }
.check { display: flex !important; align-items: center; gap: 8px; }
.find { max-width: 280px; }
.found { display: grid; gap: 10px; }
.found-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.repos { list-style: none; margin: 0; padding: 0; display: grid; max-height: 320px; overflow: auto; }
.repo { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 8px 0; border-bottom: 1px dashed var(--line); }
.repo:last-child { border-bottom: 0; }
.name { font-weight: 600; }
.desc { flex: 1 1 160px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.full { flex-basis: 100%; }
.small { font-size: 12.5px; }
.ok { color: var(--success); }
</style>
