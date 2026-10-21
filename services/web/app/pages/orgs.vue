<script setup lang="ts">
// Watch GitHub organisations: list every repository, and clone + index new
// ones as they appear (the API rescans every 5 minutes).
interface OrgRepo { name: string; repo: string; default_branch?: string; description?: string; archived?: boolean; fork?: boolean; private?: boolean; pushed_at?: string; status: string; last_error?: string }
interface Org {
  org: string; include_archived: boolean; include_forks: boolean; auto_index: boolean
  last_scan?: string; last_error?: string; next_scan?: string
  repos: OrgRepo[]; counts: Record<string, number>
}

const { data, refresh } = useApiGet<{ orgs: Org[]; enabled: boolean }>('orgs')
const { data: settings } = useApiGet<{ git_token_configured: boolean; git_token_host: string }>('settings')

const form = reactive({ org: '', include_archived: false, include_forks: false, auto_index: true })
const adding = ref(false)
const error = ref('')
async function add() {
  error.value = ''
  adding.value = true
  try {
    await apiPost('orgs', { ...form, org: form.org.trim() })
    form.org = ''
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    adding.value = false
  }
}

async function call(method: 'PUT' | 'DELETE' | 'POST', path: string, body?: Record<string, unknown>) {
  error.value = ''
  try {
    await $fetch(apiPath(path), { method, body })
    await refresh()
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
const rescan = (o: Org) => call('POST', `orgs/${encodeURIComponent(o.org)}/scan`)
const stop = (o: Org) => call('DELETE', `orgs/${encodeURIComponent(o.org)}`)
const index = (o: Org, r: OrgRepo) => call('POST', `orgs/${encodeURIComponent(o.org)}/repos/${encodeURIComponent(r.name)}/index`)
const setting = (o: Org, patch: Partial<Pick<Org, 'include_archived' | 'include_forks' | 'auto_index'>>) =>
  call('PUT', `orgs/${encodeURIComponent(o.org)}`, { include_archived: o.include_archived, include_forks: o.include_forks, auto_index: o.auto_index, ...patch })

const open = reactive<Record<string, boolean>>({})
const filter = reactive<Record<string, string>>({})
const shown = (o: Org) => o.repos.filter((r) => !filter[o.org] || r.name.toLowerCase().includes(filter[o.org]!.toLowerCase()))

const busy = computed(() => data.value?.orgs.some((o) => (o.counts.indexing ?? 0) + (o.counts.pending ?? 0) > 0) ?? false)
let timer: ReturnType<typeof setInterval> | undefined
watch(busy, (b) => {
  if (b && !timer) timer = setInterval(() => refresh(), 5000)
  if (!b && timer) { clearInterval(timer); timer = undefined }
}, { immediate: true })
onBeforeUnmount(() => timer && clearInterval(timer))

const STATUS: Record<string, { label: string; cls: string }> = {
  indexed: { label: 'indexed', cls: 'badge--green' },
  indexing: { label: 'indexing…', cls: 'badge--cyan' },
  pending: { label: 'queued', cls: 'badge--cyan' },
  available: { label: 'not indexed', cls: 'badge--violet' },
  failed: { label: 'failed', cls: 'badge--pink' },
  conflict: { label: 'name taken', cls: 'badge--amber' },
  excluded: { label: 'excluded', cls: 'badge--violet' },
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Workspace" title="Organisations" lede="Watch a GitHub organisation and CodeSynapse lists every repository in it, indexes them, and picks up new repositories automatically." />

    <p v-if="data && !data.enabled" class="error">Cloning is off: set CODESYNAPSE_WORKSPACE on the API to watch organisations.</p>

    <section class="panel edge--cyan">
      <p class="eyebrow">Watch an organisation</p>
      <h2>Index everything in <span class="mono">astrona-io/*</span></h2>
      <form class="add" @submit.prevent="add">
        <label class="grow">GitHub organisation or user
          <input v-model="form.org" required placeholder="astrona-io" autocomplete="off">
        </label>
        <label class="check"><input v-model="form.auto_index" type="checkbox"> Index new repositories automatically</label>
        <label class="check"><input v-model="form.include_archived" type="checkbox"> Include archived</label>
        <label class="check"><input v-model="form.include_forks" type="checkbox"> Include forks</label>
        <div class="row">
          <button type="submit" :disabled="!form.org.trim() || adding || data?.enabled === false">{{ adding ? 'Scanning…' : 'Scan & watch' }}</button>
          <span class="muted small">
            <template v-if="settings?.git_token_configured">Private repositories on {{ settings.git_token_host }} are included (token configured on the server).</template>
            <template v-else>Without a token only public repositories are listed. Run <code>just git-token</code> to add private ones.</template>
          </span>
        </div>
      </form>
      <p v-if="error" class="error">{{ error }}</p>
    </section>

    <section v-for="o in data?.orgs ?? []" :key="o.org" class="panel org">
      <div class="org-head">
        <div>
          <p class="eyebrow">GitHub</p>
          <h2 class="mono">{{ o.org }}/*</h2>
          <p class="muted small">
            {{ plural(o.repos.length, 'repository', 'repositories') }}
            · {{ o.counts.indexed ?? 0 }} indexed
            <template v-if="o.counts.indexing || o.counts.pending"> · {{ (o.counts.indexing ?? 0) + (o.counts.pending ?? 0) }} on the way</template>
            <template v-if="o.counts.failed"> · <span class="bad">{{ o.counts.failed }} failed</span></template>
            · scanned {{ o.last_scan ? relTime(o.last_scan) : 'never' }}
          </p>
        </div>
        <div class="row tight-actions">
          <button class="secondary tiny" @click="rescan(o)">Scan now</button>
          <button class="secondary tiny" @click="open[o.org] = !open[o.org]">{{ open[o.org] ? 'Hide repositories' : 'Show repositories' }}</button>
          <button class="secondary tiny" title="Stops watching; indexed repositories stay" @click="stop(o)">Stop watching</button>
        </div>
      </div>
      <p v-if="o.last_error" class="error small">Last scan failed: {{ o.last_error }}</p>
      <div class="row opts">
        <label class="check"><input type="checkbox" :checked="o.auto_index" @change="setting(o, { auto_index: ($event.target as HTMLInputElement).checked })"> Auto-index new</label>
        <label class="check"><input type="checkbox" :checked="o.include_archived" @change="setting(o, { include_archived: ($event.target as HTMLInputElement).checked })"> Archived</label>
        <label class="check"><input type="checkbox" :checked="o.include_forks" @change="setting(o, { include_forks: ($event.target as HTMLInputElement).checked })"> Forks</label>
      </div>

      <template v-if="open[o.org]">
        <input v-model="filter[o.org]" class="find" placeholder="Filter repositories" aria-label="Filter repositories">
        <ul class="repos">
          <li v-for="r in shown(o)" :key="r.name" class="repo">
            <NuxtLink v-if="r.status === 'indexed'" :to="`/repos/${encodeURIComponent(r.repo)}`" class="mono name" :title="r.repo">{{ r.name }}</NuxtLink>
            <span v-else class="mono name">{{ r.name }}</span>
            <span v-if="r.private" class="badge badge--violet">private</span>
            <span v-if="r.archived" class="badge badge--amber">archived</span>
            <span v-if="r.fork" class="badge badge--violet">fork</span>
            <span class="badge" :class="STATUS[r.status]?.cls">{{ STATUS[r.status]?.label ?? r.status }}</span>
            <span class="muted small desc">{{ r.description }}</span>
            <button v-if="r.status === 'available' || r.status === 'failed' || r.status === 'excluded'" class="secondary tiny" @click="index(o, r)">{{ r.status === 'failed' ? 'Retry' : 'Index' }}</button>
            <p v-if="r.status === 'failed' && r.last_error" class="error small full">{{ r.last_error }}</p>
          </li>
        </ul>
      </template>
    </section>

    <div v-if="data && !data.orgs.length" class="empty"><strong>No organisations yet</strong>Enter an organisation above, for example <code>astrona-io</code>.</div>
  </div>
</template>

<style scoped>
.add { display: grid; gap: 10px; }
.add label { display: grid; gap: 4px; }
.check { display: flex !important; align-items: center; gap: 8px; }
.grow { flex: 1 1 220px; }
.org { display: grid; gap: 12px; }
.org-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; flex-wrap: wrap; }
.opts { gap: 18px; flex-wrap: wrap; }
.find { max-width: 320px; }
.repos { list-style: none; margin: 0; padding: 0; display: grid; }
.repo { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 8px 0; border-bottom: 1px dashed var(--line); }
.repo:last-child { border-bottom: 0; }
.name { font-weight: 600; }
.desc { flex: 1 1 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.full { flex-basis: 100%; }
.bad { color: var(--pink); }
.small { font-size: 12.5px; }
</style>
