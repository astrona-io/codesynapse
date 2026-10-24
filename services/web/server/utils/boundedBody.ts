// Reads a request body without ever holding more than `limit` bytes. The
// proxy used to buffer whatever arrived before the API could refuse it, so a
// large anonymous request cost web-server memory first.

export class BodyTooLarge extends Error {
  limit: number
  constructor(limit: number) {
    super(`request body exceeds ${limit} bytes`)
    this.limit = limit
  }
}

/**
 * readBounded collects `stream` into one buffer. A Content-Length above the
 * limit is refused before a byte is read; the bytes actually received are
 * counted too, so a chunked body or a dishonest Content-Length is cut off at
 * the limit and the stream is cancelled.
 */
export async function readBounded(stream: ReadableStream<Uint8Array> | undefined | null, limit: number, contentLength?: string | null): Promise<Uint8Array<ArrayBuffer>> {
  if (contentLength) {
    const declared = Number(contentLength)
    if (!Number.isFinite(declared) || declared < 0) {
      await stream?.cancel().catch(() => undefined)
      throw new BodyTooLarge(limit) // not a length we can trust
    }
    if (declared > limit) {
      await stream?.cancel().catch(() => undefined)
      throw new BodyTooLarge(limit)
    }
  }
  if (!stream) return new Uint8Array(0)
  const reader = stream.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    total += value.byteLength
    if (total > limit) {
      await reader.cancel().catch(() => undefined)
      throw new BodyTooLarge(limit)
    }
    chunks.push(value)
  }
  const out = new Uint8Array(total)
  let at = 0
  for (const c of chunks) {
    out.set(c, at)
    at += c.byteLength
  }
  return out
}
