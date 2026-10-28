// Run with `npm test` (node's built-in test runner).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { methodLabel, pagePattern, proxyPattern, routePattern, statusClass } from './routePattern.ts'

test('proxy paths become API route patterns', () => {
  const cases = [
    ['repos', '/api/cg/repos'],
    ['repos/acme%2Fwidgets', '/api/cg/repos/{repo}'],
    ['repos/acme%2Fwidgets/security', '/api/cg/repos/{repo}/security'],
    ['repos/acme%2Fwidgets/reviews/0123abcd/comments/7', '/api/cg/repos/{repo}/reviews/*'],
    ['repos/acme%2Fwidgets/secret-plans', '/api/cg/repos/{repo}/other'],
    ['repos/acme%2Fwidgets/secret-plans/more', '/api/cg/repos/{repo}/other/*'],
    ['auth/status', '/api/cg/auth/*'],
    ['stats/series?panel=tool_calls&range=1h', '/api/cg/stats/*'],
    ['version', '/api/cg/version'],
    ['accounts/orgs/acme/members/u-123', '/api/cg/accounts/*'],
    ['acme-corp/whatever', '/api/cg/other'],
    ['', '/api/cg/other'],
  ]
  for (const [path, want] of cases) assert.equal(proxyPattern(path), want, path)
})

test('page definitions become labels; anything else is refused', () => {
  assert.equal(pagePattern('/repos/:repo()/security'), '/repos/{repo}/security')
  assert.equal(pagePattern('/repos/:repo()/architecture/:id()'), '/repos/{repo}/architecture/{id}')
  assert.equal(pagePattern('/:slug(.*)*'), '/{slug}')
  assert.equal(pagePattern('/stats/activity'), '/stats/activity')
  assert.equal(pagePattern('/'), '/')
  assert.equal(pagePattern(undefined), undefined)
  assert.equal(pagePattern('repos/x'), undefined, 'not a definition: no leading slash')
  assert.equal(pagePattern('/repos/acme widgets'), undefined, 'spaces never appear in a definition')
  assert.equal(pagePattern('/repos/acme?x=1'), undefined)
  assert.equal(pagePattern('/' + 'a'.repeat(200)), undefined, 'length is bounded')
})

test('routePattern never returns a visited path', () => {
  const visited = [
    '/repos/acme%2Fwidgets/security?tab=secrets',
    '/api/cg/repos/acme%2Fwidgets/symbol?id=pkg.Func',
    '/api/cg/me/git-tokens/github.com',
    '/settings/organisation?org=acme',
    '/totally/unknown/acme',
    '/api/nope/acme',
    '/_nuxt/entry.abc123.js',
    '/favicon.svg',
  ]
  for (const path of visited) {
    const got = routePattern(path)
    assert.ok(!got.includes('acme') && !got.includes('github') && !got.includes('?'), `${path} → ${got}`)
  }
  assert.equal(routePattern('/repos/acme%2Fwidgets/security', '/repos/:repo()/security'), '/repos/{repo}/security')
  assert.equal(routePattern('/api/cg/repos/acme%2Fwidgets/symbol?id=x'), '/api/cg/repos/{repo}/symbol')
  assert.equal(routePattern('/api/health'), '/api/health')
  assert.equal(routePattern('/api/nope'), '/api/other')
  assert.equal(routePattern('/_nuxt/entry.js'), 'static')
  assert.equal(routePattern('/favicon.svg'), 'static')
  assert.equal(routePattern('/no-such-page'), 'other')
  assert.equal(routePattern('/repos/x', 'not a definition'), 'other', 'a bad page value falls back to the path rules')
})

test('methods and statuses stay in their sets', () => {
  assert.equal(methodLabel('get'), 'GET')
  assert.equal(methodLabel('PROPFIND'), 'OTHER')
  assert.equal(methodLabel(undefined), 'OTHER')
  assert.equal(statusClass(200), '2xx')
  assert.equal(statusClass(404), '4xx')
  assert.equal(statusClass(504), '5xx')
  assert.equal(statusClass(99), 'other')
  assert.equal(statusClass(600), 'other')
  assert.equal(statusClass(undefined), 'other')
  assert.equal(statusClass(200.5), 'other')
})
