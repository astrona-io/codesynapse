import { apiErrorMessage } from '~/composables/useApi'

// Plain-language text for a failed API call. The API answers with RFC 9457
// problems whose detail is often a terse Go error ("already exists",
// "forbidden"); people should read what happened and what to do instead.
// Requests the API refused for their content (400, 409, 422) keep its
// detail, because it names the field or the conflict.
export function apiStatus(err: unknown): number | undefined {
  const e = err as { statusCode?: number; status?: number; response?: { status?: number } } | null
  return e?.statusCode ?? e?.status ?? e?.response?.status
}

const BY_STATUS: Record<number, string> = {
  401: 'Your session has ended. Sign in again.',
  403: 'You do not have permission to do this. Ask an owner or admin of your organisation.',
  404: 'Not found. It may have been removed, or you may not have access to it.',
  413: 'That is too large to send.',
  429: 'Too many requests. Wait a minute and try again.',
  500: 'Something went wrong on the server. Try again; if it keeps happening, tell whoever runs CodeSynapse.',
  502: 'CodeSynapse is not available right now. Try again in a moment.',
  503: 'CodeSynapse is not available right now. Try again in a moment.',
  504: 'CodeSynapse took too long to answer. Try again in a moment.',
}

function sentence(text: string): string {
  const t = text.trim()
  if (!t) return t
  const s = t[0]!.toUpperCase() + t.slice(1)
  return /[.!?]$/.test(s) ? s : `${s}.`
}

export function plainError(err: unknown, overrides: Partial<Record<number, string>> = {}): string {
  const status = apiStatus(err)
  if (status && overrides[status]) return overrides[status]!
  if (status === 400 || status === 409 || status === 422) {
    const detail = (err as { data?: { detail?: string } } | null)?.data?.detail
    return detail ? sentence(detail) : 'The request was not accepted. Check the fields and try again.'
  }
  if (status && BY_STATUS[status]) return BY_STATUS[status]!
  if (status && status >= 500) return BY_STATUS[500]!
  if (!status && (err as { name?: string } | null)?.name === 'FetchError') return 'Could not reach CodeSynapse. Check your connection and try again.'
  return sentence(apiErrorMessage(err))
}
