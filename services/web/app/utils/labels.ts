// Plain-language names and explanations used across the UI, so every page
// speaks the same words.

export const FORMAT_LABELS: Record<string, string> = {
  go: 'Go', python: 'Python', typescript: 'TypeScript', javascript: 'JavaScript', vue: 'Vue', java: 'Java', kotlin: 'Kotlin', rust: 'Rust', csharp: 'C#', php: 'PHP', ruby: 'Ruby',
  yaml: 'YAML', helm: 'Helm', 'go-template': 'Go templates', markdown: 'Markdown',
  'github-actions': 'GitHub Actions', compose: 'Compose', npm: 'npm lockfiles', pypi: 'Python lockfiles',
}

// Display order: code first, then config, then docs and dependency manifests.
export const FORMAT_ORDER = ['go', 'python', 'typescript', 'javascript', 'vue', 'java', 'kotlin', 'rust', 'csharp', 'php', 'ruby', 'helm', 'yaml', 'github-actions', 'compose', 'go-template', 'markdown', 'npm', 'pypi']

export const FORMAT_GROUP: Record<string, 'code' | 'config' | 'docs' | 'deps'> = {
  go: 'code', python: 'code', typescript: 'code', javascript: 'code', vue: 'code', java: 'code', kotlin: 'code', rust: 'code', csharp: 'code', php: 'code', ruby: 'code',
  yaml: 'config', helm: 'config', 'go-template': 'config', 'github-actions': 'config', compose: 'config',
  markdown: 'docs', npm: 'deps', pypi: 'deps',
}

export function formatEntries(m: Record<string, number> | undefined) {
  return FORMAT_ORDER.filter((k) => (m ?? {})[k]).map((k) => ({ key: k, label: FORMAT_LABELS[k] ?? k, count: m![k]!, group: FORMAT_GROUP[k] ?? 'code' }))
}

export const GRADE_TEXT: Record<string, string> = {
  A: 'In good shape',
  B: 'Mostly fine, a few things to fix',
  C: 'Needs attention',
  D: 'Serious issues to address',
  F: 'Critical issues — act now',
  'N/A': 'Nothing here is covered by the security rules yet',
}

export const SEVERITY_TEXT: Record<string, string> = {
  critical: 'Exploitable and severe — fix first',
  high: 'Likely exploitable — fix soon',
  medium: 'Weakens security — plan a fix',
  low: 'Hardening opportunity',
  info: 'For awareness',
}

export const REACH_TEXT: Record<string, string> = {
  called: 'Your code calls the vulnerable function',
  imported: 'Your code imports the affected package, but not the vulnerable function',
  required: 'Only listed as a dependency; your code does not use the affected package',
}

export const GLOSSARY: Record<string, string> = {
  exposed: 'Reachable from an entry point — main, an exported API, or an HTTP/CLI handler — so outside input can reach it.',
  tested: 'At least one test calls this code within three hops.',
  reachability: 'How directly your code uses a vulnerable dependency: called, imported, or only required.',
  'test-only': 'Production code never calls it; only tests do. It still ships with the build.',
  unreachable: 'Nothing calls or references it from any entry point. Safe to remove after a quick check.',
  confidence: 'How sure the analysis is: high means unexported and unused, low means it could be used by reflection or from outside the repo.',
  hotspot: 'Complex code that changes often. Bugs concentrate here.',
  reduction: 'How much smaller a graph answer is than the files an agent would otherwise read for the same question.',
  instability: 'Ce / (Ca + Ce): 0 means many packages depend on it and it depends on few (stable), 1 means the opposite.',
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n.toLocaleString('en-US')} ${n === 1 ? one : many}`
}
