// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { announce, orderNodes, stepSelection } from './graphNav.ts'

const nodes = [
  { id: 'c', name: 'Charlie', kind: 'Function' },
  { id: 'a', name: 'alpha', kind: 'Type' },
  { id: 'b', name: 'Bravo', kind: 'Function' },
  { id: 'hub', name: 'Zulu', kind: 'Package' },
]
const edges = [{ from: 'hub', to: 'a' }, { from: 'hub', to: 'b' }, { from: 'hub', to: 'c' }, { from: 'a', to: 'b' }]

test('hubs come first, then names, the same way every time', () => {
  assert.deepEqual(orderNodes(nodes, edges).map((n) => n.id), ['hub', 'a', 'b', 'c'])
  assert.deepEqual(orderNodes([...nodes].reverse(), edges).map((n) => n.id), ['hub', 'a', 'b', 'c'])
  assert.deepEqual(nodes.map((n) => n.id), ['c', 'a', 'b', 'hub'], 'the input is not reordered')
  assert.deepEqual(orderNodes(nodes, []).map((n) => n.id), ['a', 'b', 'c', 'hub'], 'no edges: by name')
})

test('stepping starts at either end and stops at the ends', () => {
  const order = orderNodes(nodes, edges)
  assert.equal(stepSelection(order, null, 1), 'hub')
  assert.equal(stepSelection(order, null, -1), 'c')
  assert.equal(stepSelection(order, 'hub', 1), 'a')
  assert.equal(stepSelection(order, 'a', -1), 'hub')
  assert.equal(stepSelection(order, 'hub', -1), 'hub')
  assert.equal(stepSelection(order, 'c', 1), 'c')
  assert.equal(stepSelection(order, 'gone', 1), 'hub', 'a node that is no longer shown starts the walk over')
  assert.equal(stepSelection([], 'a', 1), null)
})

test('what a screen reader hears', () => {
  assert.equal(announce({ busy: true, nodes: 3, edges: 2 }), 'Loading the graph')
  assert.equal(announce({ busy: false, nodes: 40, edges: 53 }), '40 nodes and 53 edges loaded')
  assert.equal(announce({ busy: false, nodes: 1, edges: 1 }), '1 node and 1 edge loaded')
  assert.equal(announce({ busy: false, nodes: 4, edges: 4, selected: nodes[0], connections: 1 }), 'Selected Charlie, Function, 1 connection')
  assert.equal(announce({ busy: false, nodes: 4, edges: 4, selected: nodes[0], connections: 12 }), 'Selected Charlie, Function, 12 connections')
})
