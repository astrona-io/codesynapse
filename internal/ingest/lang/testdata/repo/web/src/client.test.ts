import { describe, it, expect } from 'vitest'
import { fetchUser } from './api/client'

describe('client', () => {
  it('fetches', async () => {
    expect(await fetchUser('1')).toBeDefined()
  })
})
