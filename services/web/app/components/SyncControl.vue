<script setup lang="ts">
// Branch picker + Sync button. Clones registered by URL fetch the chosen
// branch from origin; mounted sources re-index what is on disk. Polls the
// job and refreshes every page's data when it finishes.
import type { Job } from '~/utils/types'
import { createPoller, type PollStatus } from '~/utils/jobPolling'

const props = defineProps<{ repo: string }>()
const { data: info, refresh: refreshBranches } = useApiGet<{ current: string; branches: string[]; managed: boolean; remote?: string }>(
  () => repoPath(props.repo, 'branches'),
)
const branch = ref('')
watch(info, (i) => { if (i && !branch.value) branch.value = i.current || i.branches[0] || '' }, { immediate: true })

const job = ref<Job | null>(null)
const error = ref('')
// Whether we can see the job is separate from the job's own state: a dropped
// connection must not read as a failed job, nor free the button for a second
// one while the first still runs.
const pollStatus = ref<PollStatus>('stopped')
let poller: ReturnType<typeof createPoller<Job>> | undefined

const status = (e: unknown) => (e as { statusCode?: number; response?: { status?: number } })?.statusCode ?? (e as { response?: { status?: number } })?.response?.status

function watchJob(id: string) {
  poller?.stop()
  poller = createPoller<Job>({
    fetch: () => $fetch<Job>(apiPath(`jobs/${id}`)),
    done: (j) => j.state !== 'running',
    onUpdate: (j) => { job.value = j },
    onFinish: async (j) => {
      if (j.state === 'succeeded') {
        await refreshNuxtData()
        await refreshBranches()
      } else {
        error.value = j.error ?? 'sync failed'
      }
    },
    onStatus: (s) => { pollStatus.value = s },
    // The job is gone or access was lost: nothing to wait for.
    fatal: (e) => [401, 403, 404].includes(status(e) ?? 0),
    onError: (e) => {
      error.value = apiErrorMessage(e)
      if (job.value) job.value = { ...job.value, state: 'failed' }
    },
  })
  poller.start()
}

async function sync() {
  error.value = ''
  try {
    const body = info.value?.managed ? { branch: branch.value } : {}
    job.value = await apiPost<Job>(repoPath(props.repo, 'sync'), body)
    watchJob(job.value.id)
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
onBeforeUnmount(() => poller?.stop())
const lost = computed(() => pollStatus.value === 'lost')
const running = computed(() => job.value?.state === 'running')
</script>

<template>
  <div class="sync">
    <select v-if="info?.branches.length" v-model="branch" :disabled="running" aria-label="Branch" :title="info.managed ? '' : 'Mounted source: only the checked-out branch can be synced'">
      <option v-for="b in info.branches" :key="b" :value="b" :disabled="!info.managed && b !== info.current">{{ b }}{{ b === info.current ? ' (indexed)' : '' }}</option>
    </select>
    <span v-else-if="info?.current" class="badge badge--violet" title="Mounted source: follows the checkout on disk">{{ info.current }}</span>
    <button :disabled="running" :title="info?.managed ? `Fetch ${branch} from ${info.remote} and re-index` : 'Re-index the mounted checkout'" @click="sync">
      {{ running ? (pollStatus === 'retrying' ? 'Reconnecting…' : 'Syncing…') : 'Sync' }}
    </button>
    <template v-if="lost">
      <span class="warn small" role="status">Lost contact with the server. The sync may still be running.</span>
      <button type="button" class="secondary" @click="poller?.resume()">Check status</button>
    </template>
    <span v-if="job && job.state === 'succeeded'" class="badge badge--green">synced {{ fmtInt(job.nodes) }} nodes</span>
    <span v-if="error" class="error small" :title="error">{{ error.length > 80 ? `${error.slice(0, 79)}…` : error }}</span>
  </div>
</template>

<style scoped>
.sync { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.sync select { width: auto; min-width: 150px; min-height: 38px; }
.small { font-size: 12.5px; }
.warn { color: var(--warning); }
</style>
