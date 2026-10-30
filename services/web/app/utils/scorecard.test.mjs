// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { scorecardBadge, scorecardOfNode, scorecardTitle } from './scorecard.ts'

test('badge tint by band', () => {
  assert.equal(scorecardBadge(0), 'badge badge--pink')
  assert.equal(scorecardBadge(3.9), 'badge badge--pink')
  assert.equal(scorecardBadge(4), 'badge badge--amber')
  assert.equal(scorecardBadge(6.9), 'badge badge--amber')
  assert.equal(scorecardBadge(7), 'badge badge--green')
})

test('hover text names the lowest checks', () => {
  assert.equal(scorecardTitle({ score: 3.7, date: '2026-10-19', checks: [{ name: 'Code-Review', score: 0 }, { name: 'Maintained', score: 2 }] }),
    'OpenSSF Scorecard 3.7 of 10, scored on 2026-10-19. Lowest checks: Code-Review 0, Maintained 2')
  assert.equal(scorecardTitle({ score: 8, date: '2026-10-19' }), 'OpenSSF Scorecard 8.0 of 10, scored on 2026-10-19')
})

test('graph node scorecard', () => {
  assert.equal(scorecardOfNode({ scorecard: 5 }), undefined, 'no date, no score')
  assert.deepEqual(scorecardOfNode({ scorecard_date: '2026-10-19', scorecard_checks: ['Code-Review:0', 'broken', 'SAST:x', 'Fuzzing:'] }),
    { score: 0, date: '2026-10-19', checks: [{ name: 'Code-Review', score: 0 }] })
})
