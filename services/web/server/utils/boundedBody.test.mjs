// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { BodyTooLarge, readBounded } from './boundedBody.ts'

function streamOf(chunks, pulled) {
  let i = 0
  return new ReadableStream({
    pull(controller) {
      if (pulled) pulled.n++
      if (i < chunks.length) controller.enqueue(chunks[i++])
      else controller.close()
    },
  })
}

const bytes = (n, fill = 1) => new Uint8Array(n).fill(fill)

test('a body within the limit is returned whole, in order', async () => {
  const out = await readBounded(streamOf([bytes(3, 1), bytes(2, 2)]), 10)
  assert.deepEqual([...out], [1, 1, 1, 2, 2])
  assert.equal((await readBounded(streamOf([bytes(10)]), 10)).byteLength, 10, 'exactly the limit is allowed')
  assert.equal((await readBounded(null, 10)).byteLength, 0)
  assert.equal((await readBounded(streamOf([]), 10)).byteLength, 0)
  assert.equal((await readBounded(streamOf([bytes(4)]), 10, '4')).byteLength, 4, 'an honest Content-Length passes')
})

test('an honest Content-Length above the limit is refused before reading', async () => {
  const pulled = { n: 0 }
  await assert.rejects(readBounded(streamOf([bytes(100)], pulled), 10, '100'), BodyTooLarge)
  assert.equal(pulled.n, 0, 'not a byte was read')
})

test('a chunked body is cut off at the limit and the stream is cancelled', async () => {
  const pulled = { n: 0 }
  const chunks = Array.from({ length: 1000 }, () => bytes(10))
  await assert.rejects(readBounded(streamOf(chunks, pulled), 25), BodyTooLarge)
  assert.ok(pulled.n <= 5, `stopped early, pulled ${pulled.n} chunks`)
})

test('a Content-Length that understates the body does not help', async () => {
  await assert.rejects(readBounded(streamOf([bytes(50)]), 10, '5'), BodyTooLarge)
  await assert.rejects(readBounded(streamOf([bytes(50)]), 10, 'not-a-number'), BodyTooLarge)
  await assert.rejects(readBounded(streamOf([bytes(5)]), 10, '-1'), BodyTooLarge)
})
