// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { gatewayReasonLabel, gatewayReasonRows, gatewayRouteLabel } from './gatewayReasons.ts'

test('known reasons read as plain language, new codes still show', () => {
  assert.equal(gatewayReasonLabel('queue_timeout'), 'waited too long for a slot')
  assert.equal(gatewayReasonLabel('count_tokens'), 'count tokens', 'a code the UI does not know yet is spelled out')
  assert.equal(gatewayReasonLabel('not-eligible_tools'), 'not eligible tools')
  assert.equal(gatewayReasonLabel(''), 'unknown')
  assert.equal(gatewayRouteLabel('local'), 'local model')
  assert.equal(gatewayRouteLabel('shadow'), 'shadow')
})

test('reasons are ranked largest first, ties by code, zeros left out', () => {
  const rows = gatewayReasonRows({ too_large: 4, brand_new: 4, queue_timeout: 9, has_image: 0 })
  assert.deepEqual(rows.map((r) => r.detail), ['queue_timeout', 'brand_new', 'too_large'])
  assert.equal(rows[1].label, 'brand new')
  assert.deepEqual(gatewayReasonRows(undefined), [])
})
