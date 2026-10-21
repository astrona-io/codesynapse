import { formatName } from '../util'
import axios from 'axios'

/** Fetches a user from the API. */
export async function fetchUser(id: string): Promise<string> {
  const res = await axios.get(`/api/users/${id}`)
  return formatName(res.data.name)
}

export class Client {
  private token = 'abc'

  constructor(private base: string) {}

  async list(): Promise<string[]> {
    const r = await fetch(this.base + '/list')
    return this.parse(await r.text())
  }

  parse(body: string): string[] {
    // a "string" with { braces } and `template` inside
    const re = /\{[a-z]+\}/g
    return body.split(',').filter((x) => re.test(x) || x.length > 0)
  }
}

export const render = (el: HTMLElement, html: string) => {
  el.innerHTML = html
}
