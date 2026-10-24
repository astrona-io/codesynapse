// Polls one server job until it ends. A failed poll (a dropped connection, a
// 500) says nothing about the job, which keeps running on the server, so it
// is retried with a growing delay and then left to the person ("Check
// status") instead of being turned into a job state or a second job.

export type PollStatus = 'polling' | 'retrying' | 'lost' | 'stopped'

export interface PollerOptions<J> {
  fetch: () => Promise<J>
  // done says the job reached an end state; onFinish then runs once.
  done: (job: J) => boolean
  onUpdate: (job: J) => void
  onFinish: (job: J) => void | Promise<void>
  onStatus: (status: PollStatus) => void
  // fatal marks an error that retrying cannot fix (the job is gone, access
  // was lost); the poller stops and reports it through onError.
  fatal?: (error: unknown) => boolean
  onError?: (error: unknown) => void
  intervalMs?: number
  // Delays before each retry of consecutive failures; running out of them
  // moves to 'lost'.
  retryDelaysMs?: number[]
  schedule?: (fn: () => void, ms: number) => unknown
  cancel?: (handle: unknown) => void
}

export function createPoller<J>(o: PollerOptions<J>) {
  const interval = o.intervalMs ?? 1500
  const delays = o.retryDelaysMs ?? [1500, 3000, 6000, 12000, 20000]
  const schedule = o.schedule ?? ((fn: () => void, ms: number) => setTimeout(fn, ms))
  const cancel = o.cancel ?? ((h: unknown) => clearTimeout(h as ReturnType<typeof setTimeout>))
  let handle: unknown
  let failures = 0
  let generation = 0 // a stop or resume makes older in-flight ticks harmless

  async function tick(gen: number) {
    handle = undefined
    let job: J
    try {
      job = await o.fetch()
    } catch (e) {
      if (gen !== generation) return
      if (o.fatal?.(e)) {
        o.onStatus('stopped')
        o.onError?.(e)
        return
      }
      failures++
      if (failures > delays.length) {
        o.onStatus('lost')
        return
      }
      o.onStatus('retrying')
      handle = schedule(() => void tick(gen), delays[failures - 1]!)
      return
    }
    if (gen !== generation) return
    failures = 0
    o.onStatus('polling')
    o.onUpdate(job)
    if (o.done(job)) {
      o.onStatus('stopped')
      await o.onFinish(job)
      return
    }
    handle = schedule(() => void tick(gen), interval)
  }

  function stop() {
    generation++
    if (handle !== undefined) cancel(handle)
    handle = undefined
    o.onStatus('stopped')
  }

  return {
    // start polls after one interval; resume (also "Check status") polls now.
    start() {
      generation++
      failures = 0
      o.onStatus('polling')
      const gen = generation
      handle = schedule(() => void tick(gen), interval)
    },
    resume() {
      generation++
      failures = 0
      if (handle !== undefined) cancel(handle)
      o.onStatus('polling')
      void tick(generation)
    },
    stop,
  }
}
