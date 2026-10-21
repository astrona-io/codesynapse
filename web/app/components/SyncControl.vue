<script setup lang="ts">
// Branch picker + Sync button. Clones registered by URL fetch the chosen
// branch from origin; mounted sources re-index what is on disk. Polls the
// job and refreshes every page's data when it finishes.
import type { Job } from '~/utils/types'

const props = defineProps<{ repo: string }>()
const { data: info, refresh: refreshBranches } = useApiGet<{ current: string; branches: string[]; managed: boolean; remote?: string }>(
  () => repoPath(props.repo, 'branches'),
)
const branch = ref('')
watch(info, (i) => { if (i && !branch.value) branch.value = i.current || i.branches[0] || '' }, { immediate: true })

const job = ref<Job | null>(null)
const error = ref('')
let timer: ReturnType<typeof setInterval> | undefined

async function poll(id: string) {
  try {
    job.value = await $fetch<Job>(apiPath(`jobs/${id}`))
    if (job.value.state !== 'running') {
      clearInterval(timer)
      timer = undefined
      if (job.value.state === 'succeeded') {
        await refreshNuxtData()
        await refreshBranches()
      } else {
        error.value = job.value.error ?? 'sync failed'
      }
    }
  } catch (e) {
    error.value = apiErrorMessage(e)
    clearInterval(timer)
  }
}

async function sync() {
  error.value = ''
  try {
    const body = info.value?.managed ? { branch: branch.value } : {}
    job.value = await apiPost<Job>(repoPath(props.repo, 'sync'), body)
    timer = setInterval(() => poll(job.value!.id), 1500)
  } catch (e) {
    error.value = apiErrorMessage(e)
  }
}
onBeforeUnmount(() => timer && clearInterval(timer))
const running = computed(() => job.value?.state === 'running')
</script>

<template>
  <div class="sync">
    <select v-if="info?.managed && info.branches.length" v-model="branch" :disabled="running" aria-label="Branch">
      <option v-for="b in info.branches" :key="b" :value="b">{{ b }}</option>
    </select>
    <span v-else-if="info?.current" class="badge badge--violet" title="Mounted source: follows the checkout on disk">{{ info.current }}</span>
    <button :disabled="running" :title="info?.managed ? `Fetch ${branch} from ${info.remote} and re-index` : 'Re-index the mounted checkout'" @click="sync">
      {{ running ? 'Syncing…' : 'Sync' }}
    </button>
    <span v-if="job && job.state === 'succeeded'" class="badge badge--green">synced {{ fmtInt(job.nodes) }} nodes</span>
    <span v-if="error" class="error small" :title="error">{{ error.length > 80 ? `${error.slice(0, 79)}…` : error }}</span>
  </div>
</template>

<style scoped>
.sync { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.sync select { width: auto; min-width: 150px; min-height: 38px; }
.small { font-size: 12.5px; }
</style>
