// Plain-language names for the gateway's fall-through reasons and routes.
// The API sends whatever codes the gateway records, so a code added later
// still shows: unknown ones are spelled out from the code itself.

const REASONS: Record<string, string> = {
  queue_timeout: 'waited too long for a slot', answer_timeout: 'answered too slowly', cancelled: 'client gave up',
  model_error: 'model error', bad_status: 'error status', empty_answer: 'empty answer', truncated: 'cut off', bad_answer: 'unreadable answer',
  not_eligible: 'not eligible', too_large: 'too large', has_thinking: 'needs thinking', has_image: 'has an image or document',
  bad_request: 'unreadable request', system_not_text: 'system prompt not text', unsupported_block: 'unsupported content', unsupported_tool: 'unsupported tool',
  no_local_model: 'no local model', local_first_off: 'local first off', excluded: 'excluded', count_tokens: 'token count (cloud only)', other: 'other',
}

const ROUTES: Record<string, string> = { local: 'local model', cloud: 'cloud', rejected: 'refused', other: 'other' }

// humanize turns an unknown code such as "count_tokens" into "count tokens".
function humanize(code: string): string {
  const s = code.replace(/[_-]+/g, ' ').trim()
  return s || 'unknown'
}

export function gatewayReasonLabel(code: string): string {
  return REASONS[code] ?? humanize(code)
}

export function gatewayRouteLabel(code: string): string {
  return ROUTES[code] ?? humanize(code)
}

// gatewayReasonRows ranks reasons by count, largest first, ties by code so
// the order is stable between loads.
export function gatewayReasonRows(reasons: Record<string, number> | undefined): { label: string; value: number; detail: string }[] {
  return Object.entries(reasons ?? {})
    .filter(([, v]) => v > 0)
    .sort((p, q) => q[1] - p[1] || p[0].localeCompare(q[0]))
    .map(([k, v]) => ({ label: gatewayReasonLabel(k), value: v, detail: k }))
}
