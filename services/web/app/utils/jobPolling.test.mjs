// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createPoller } from './jobPolling.ts'

// A scheduler the test drives by hand.
function fakeClock() {
  const queue = []
  return {
    schedule: (fn, ms) => { const h = { fn, ms }; queue.push(h); return h },
    cancel: (h) => { const i = queue.indexOf(h); if (i >= 0) queue.splice(i, 1) },
    queue,
    async next() {
      const h = queue.shift()
      assert.ok(h, 'something is scheduled')
      h.fn()
      await new Promise((r) => setImmediate(r))
      return h.ms
    },
  }
}

function harness(results, extra = {}) {
  const clock = fakeClock()
  const log = { status: [], updates: [], finished: [], errors: [] }
  let i = 0
  const poller = createPoller({
    fetch: async () => {
      const r = results[Math.min(i++, results.length - 1)]
      if (r instanceof Error) throw r
      return r
    },
    done: (j) => j.state !== 'running',
    onUpdate: (j) => log.updates.push(j.state),
    onFinish: (j) => { log.finished.push(j.state) },
    onStatus: (s) => log.status.push(s),
    onError: (e) => log.errors.push(e.message),
    schedule: clock.schedule,
    cancel: clock.cancel,
    intervalMs: 100,
    retryDelaysMs: [10, 20, 40],
    ...extra,
  })
  return { poller, clock, log, calls: () => i }
}

const running = { state: 'running' }

test('polls until the job ends, then finishes once', async () => {
  const h = harness([running, running, { state: 'succeeded' }])
  h.poller.start()
  assert.equal(await h.clock.next(), 100)
  assert.equal(await h.clock.next(), 100)
  await h.clock.next()
  assert.deepEqual(h.log.updates, ['running', 'running', 'succeeded'])
  assert.deepEqual(h.log.finished, ['succeeded'])
  assert.equal(h.clock.queue.length, 0, 'nothing is scheduled after the end')
})

test('a failed poll is retried with growing delays and recovers', async () => {
  const h = harness([running, new Error('network'), new Error('500'), running, { state: 'failed' }])
  h.poller.start()
  await h.clock.next() // running
  assert.equal(await h.clock.next(), 100) // network error
  assert.equal(h.log.status.at(-1), 'retrying')
  assert.equal(await h.clock.next(), 10)  // retry fails again: next delay is longer
  assert.equal(await h.clock.next(), 20)  // recovers: running
  assert.equal(h.log.status.at(-1), 'polling')
  assert.equal(await h.clock.next(), 100) // back to the normal interval
  assert.deepEqual(h.log.finished, ['failed'], 'the server decides failed, not the transport')
})

test('running out of retries leaves the job running and offers a manual check', async () => {
  const h = harness([running, new Error('x'), new Error('x'), new Error('x'), new Error('x')])
  h.poller.start()
  await h.clock.next()
  await h.clock.next() // 1st failure -> retry in 10
  await h.clock.next() // 2nd -> 20
  await h.clock.next() // 3rd -> 40
  await h.clock.next() // 4th: out of delays
  assert.equal(h.log.status.at(-1), 'lost')
  assert.equal(h.clock.queue.length, 0, 'it stops by itself')
  assert.deepEqual(h.log.finished, [])
  assert.ok(!h.log.updates.includes('failed'), 'the job is not declared failed')
})

test('resume checks the status at once and recovers', async () => {
  const h = harness([running, new Error('x'), new Error('x'), new Error('x'), new Error('x'), { state: 'succeeded' }])
  h.poller.start()
  for (let n = 0; n < 5; n++) await h.clock.next()
  assert.equal(h.log.status.at(-1), 'lost')
  h.poller.resume()
  await new Promise((r) => setImmediate(r))
  assert.deepEqual(h.log.finished, ['succeeded'])
})

test('a fatal error stops and is reported', async () => {
  const gone = new Error('job not found')
  const h = harness([running, gone], { fatal: (e) => e === gone })
  h.poller.start()
  await h.clock.next()
  await h.clock.next()
  assert.deepEqual(h.log.errors, ['job not found'])
  assert.equal(h.log.status.at(-1), 'stopped')
  assert.equal(h.clock.queue.length, 0)
})

test('stop cancels what is scheduled and ignores a poll still in flight', async () => {
  const h = harness([running])
  h.poller.start()
  h.poller.stop()
  assert.equal(h.clock.queue.length, 0)
  assert.equal(h.log.status.at(-1), 'stopped')
  assert.equal(h.calls(), 0)
})
