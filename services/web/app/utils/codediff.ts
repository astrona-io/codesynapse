export interface CodeLine { n: number; text: string }
export interface CommentCode { lines?: CodeLine[]; ref?: string; url?: string; error?: string }
export interface DiffRow { kind: ' ' | '-' | '+'; old?: number; new?: number; text: string }

// The first fenced code block of a suggestion is taken as replacement code for
// the commented lines; prose-only suggestions have none.
export function replacementCode(suggestion?: string): string[] | null {
  const m = /```[^\n]*\n([\s\S]*?)```/.exec(suggestion ?? '')
  if (!m) return null
  return m[1]!.replace(/\n$/, '').split('\n')
}

// suggestionDiff applies the replacement to lines [line, end] of the window and
// returns a line diff (LCS) of the window before and after, so unchanged lines
// inside the replacement show as context instead of remove + add.
export function suggestionDiff(lines: CodeLine[], line: number, end: number, replacement: string[]): DiffRow[] {
  const before = lines.map((l) => l.text)
  const first = lines.findIndex((l) => l.n === line)
  if (first < 0) return []
  const last = Math.max(first, lines.findIndex((l) => l.n === end))
  const after = [...before.slice(0, first), ...replacement, ...before.slice(last + 1)]
  const a = before.length
  const b = after.length
  const lcs: number[][] = Array.from({ length: a + 1 }, () => new Array<number>(b + 1).fill(0))
  for (let i = a - 1; i >= 0; i--) {
    for (let j = b - 1; j >= 0; j--) {
      lcs[i]![j] = before[i] === after[j] ? lcs[i + 1]![j + 1]! + 1 : Math.max(lcs[i + 1]![j]!, lcs[i]![j + 1]!)
    }
  }
  const rows: DiffRow[] = []
  const start = lines[0]!.n
  let i = 0
  let j = 0
  while (i < a || j < b) {
    if (i < a && j < b && before[i] === after[j]) {
      rows.push({ kind: ' ', old: start + i, new: start + j, text: before[i]! })
      i++; j++
    } else if (i < a && (j >= b || lcs[i + 1]![j]! >= lcs[i]![j + 1]!)) {
      rows.push({ kind: '-', old: start + i, text: before[i]! })
      i++
    } else {
      rows.push({ kind: '+', new: start + j, text: after[j]! })
      j++
    }
  }
  return rows
}

// forgeName labels a link by its host ("github.com" → "GitHub").
export function forgeName(url: string): string {
  try {
    const host = new URL(url).hostname
    return ({ 'github.com': 'GitHub', 'gitlab.com': 'GitLab', 'codeberg.org': 'Codeberg' } as Record<string, string>)[host] ?? host
  } catch {
    return 'forge'
  }
}
