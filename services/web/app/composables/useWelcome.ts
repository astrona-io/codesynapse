// The welcome guide: a short checklist shown after sign-in that a new user
// can follow or skip. Skipping and manual ticks are kept per user in this
// browser (a convenience, not a record); steps that can be detected (an
// organisation exists, a repository was added) tick themselves.
const KEY = 'cs_welcome_'

interface Saved { skipped?: boolean; done?: string[] }

export function useWelcome() {
  const session = useSession()
  const saved = useState<Saved>('welcome-saved', () => ({}))
  const key = computed(() => KEY + (session.me.value?.user.id ?? 'anonymous'))

  function load() {
    try { saved.value = JSON.parse(localStorage.getItem(key.value) ?? '{}') as Saved } catch { saved.value = {} }
  }
  function persist() {
    try { localStorage.setItem(key.value, JSON.stringify(saved.value)) } catch { /* kept for this page only */ }
  }
  onMounted(load)
  watch(key, load)

  const skipped = computed(() => !!saved.value.skipped)
  const isDone = (id: string) => !!saved.value.done?.includes(id)
  function skip() { saved.value = { ...saved.value, skipped: true }; persist() }
  function reopen() { saved.value = { ...saved.value, skipped: false }; persist() }
  function markDone(id: string) {
    if (!isDone(id)) saved.value = { ...saved.value, done: [...(saved.value.done ?? []), id] }
    persist()
  }
  return { skipped, isDone, skip, reopen, markDone }
}
