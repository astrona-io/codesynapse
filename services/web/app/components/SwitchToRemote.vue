<script setup lang="ts">
// Turns a mounted repository into a clone managed by CodeSynapse.
const props = defineProps<{ repo: string; remote?: string; branch?: string; small?: boolean }>()
const emit = defineEmits<{ switched: [] }>()
const { data: settings } = useApiGet<{ git_token_configured: boolean; git_token_host: string; clone_enabled: boolean }>('settings')

const open = ref(false)
const url = ref('')
const branch = ref('')
const busy = ref(false)
const error = ref('')
const jobState = ref<'' | 'running' | 'succeeded' | 'failed'>('')
const jobError = ref('')

// Show ssh remotes in the https form CodeSynapse will use.
function httpsForm(raw?: string) {
  const s = (raw ?? '').trim()
  const m = s.match(/^(?:ssh:\/\/)?[^@/\s]+@([^:/\s]+)(?::\d+)?[:/](.+)$/)
  return m ? `https://${m[1]}/${m[2]}` : s
}
function start() {
  url.value = httpsForm(props.remote)
  branch.value = props.branch ?? ''
  error.value = ''
  open.value = true
}

let poll: ReturnType<typeof setInterval> | undefined
onBeforeUnmount(() => clearInterval(poll))
async function confirm() {
  busy.value = true
  error.value = ''
  try {
    const job = await $fetch<{ id: string }>(apiPath(`repos/${encodeURIComponent(props.repo)}/remote`), { method: 'POST', body: { url: url.value.trim(), branch: branch.value.trim() } })
    open.value = false
    jobState.value = 'running'
    poll = setInterval(async () => {
      try {
        const j = await $fetch<{ state: string; error?: string }>(apiPath(`jobs/${job.id}`))
        if (j.state === 'running' || j.state === 'queued') return
        clearInterval(poll)
        jobState.value = j.state === 'succeeded' ? 'succeeded' : 'failed'
        jobError.value = j.error ?? ''
        if (j.state === 'succeeded') emit('switched')
      } catch { /* keep polling */ }
    }, 3000)
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <span class="switch">
    <button v-if="jobState !== 'running'" class="secondary" :class="{ tiny: small }" :disabled="settings && !settings.clone_enabled" title="Clone it from GitHub so CodeSynapse can sync branches and push fix branches" @click.prevent.stop="start">Switch to clone</button>
    <span v-else class="pill small" role="status"><span class="dot dot--warn" />Cloning and indexing…</span>
    <span v-if="jobState === 'failed'" class="error small" role="alert" :title="jobError">switch failed</span>
  </span>
  <ConfirmDialog :open="open" :title="`Clone ${repo} from its remote?`" confirm-label="Clone and switch" :busy="busy" @confirm="confirm" @cancel="open = false">
    <p>Right now CodeSynapse reads <strong>{{ repo }}</strong> from your <code>./sources</code> folder, which it can't write to. Switching clones the repository into CodeSynapse's own workspace and indexes it from there, under the same name.</p>
    <label class="field">Repository URL
      <input v-model="url" placeholder="https://github.com/owner/repo.git" autocomplete="off" spellcheck="false">
    </label>
    <label class="field">Branch
      <input v-model="branch" placeholder="default branch" autocomplete="off" spellcheck="false">
    </label>
    <ul class="facts">
      <li><strong>Kept:</strong> reviews and your decisions, auto-sync settings.</li>
      <li><strong>New:</strong> sync any branch straight from the remote, and fix branches with pull requests for accepted review items.</li>
      <li><strong>Untouched:</strong> your folder in <code>./sources</code>. You can delete it afterwards.</li>
    </ul>
    <p v-if="settings && !settings.git_token_configured" class="warn small">No git token is set. Public repositories clone fine; private ones and fix branches need <code>just git-token</code>.</p>
    <p v-if="error" class="error small" role="alert">{{ error }}</p>
  </ConfirmDialog>
</template>

<style scoped>
.switch { display: inline-flex; gap: 8px; align-items: center; }
.field { display: grid; gap: 4px; font-size: 13px; color: var(--muted); margin-bottom: 10px; }
.field input { font-family: var(--font-mono); font-size: 13px; }
.facts { margin: 4px 0 10px; padding-left: 18px; display: grid; gap: 4px; line-height: 1.5; }
.warn { color: var(--warning); }
.small { font-size: 12.5px; }
</style>
