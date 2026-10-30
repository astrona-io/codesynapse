// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { queueTabKey, queueTabOf } from './queueTabs.ts'

test('the model in the link opens its tab only when the answer lists it', () => {
  const names = ['local', 'gpu-2']
  const cases = [
    ['local', 'local'],
    [['gpu-2', 'local'], 'gpu-2'],
    ['gone', ''],
    [undefined, ''],
    [null, ''],
    ['', ''],
  ]
  for (const [query, want] of cases) assert.equal(queueTabOf(query, names), want, String(query))
})

test('arrow keys, Home and End move between tabs and wrap', () => {
  const ids = ['', 'local', 'gpu-2']
  const cases = [
    ['', 'ArrowRight', 'local'],
    ['gpu-2', 'ArrowRight', ''],
    ['', 'ArrowLeft', 'gpu-2'],
    ['local', 'Home', ''],
    ['local', 'End', 'gpu-2'],
    ['local', 'Enter', null],
    ['unknown', 'ArrowRight', 'local'],
  ]
  for (const [from, key, want] of cases) assert.equal(queueTabKey(ids, from, key), want, `${from} ${key}`)
  assert.equal(queueTabKey([], '', 'ArrowRight'), null)
})
