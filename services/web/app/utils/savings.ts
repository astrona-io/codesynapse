import type { ReferencePrice } from './types'

// What the local models' work would have cost at the reference cloud model,
// split by how the tokens would have been billed: fresh input, prompt-cache
// reads (at the cache price, else the input price) and output. The money
// tiles of Statistics → Models & savings and the savings chart both use it,
// so the chart's buckets add up to the tiles.
export function localSaved(p: ReferencePrice, input: number, cached: number, output: number) {
  const c = Math.min(cached, input)
  return {
    input: ((input - c) * p.input_per_mtok) / 1e6,
    cached: (c * (p.cached_input_per_mtok || p.input_per_mtok)) / 1e6,
    output: (output * p.output_per_mtok) / 1e6,
  }
}

// totalSaved adds the parts of "Total saved": local model work plus the
// dollars the server valued (graph answers, shorter prompts, the gateway's
// local answers). A part the server could not value (null) stays out; with
// no reference price there is no total.
export function totalSaved(p: ReferencePrice | null | undefined, local: { input: number; cached: number; output: number }, valued: (number | null | undefined)[]): number | null {
  if (!p) return null
  const l = localSaved(p, local.input, local.cached, local.output)
  return l.input + l.cached + l.output + valued.reduce<number>((t, v) => t + (v ?? 0), 0)
}
