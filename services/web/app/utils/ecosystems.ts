// The language behind each package ecosystem, for grouping dependencies.
const LANGUAGES: Record<string, string> = {
  Go: 'Go',
  npm: 'JavaScript and TypeScript',
  PyPI: 'Python',
  'crates.io': 'Rust',
  Packagist: 'PHP',
  RubyGems: 'Ruby',
  NuGet: '.NET',
  Maven: 'Java and Kotlin',
}

/** "Python (PyPI)"; an unknown ecosystem is shown as it is. */
export function ecosystemLabel(ecosystem: string): string {
  const lang = LANGUAGES[ecosystem]
  return !lang || lang === ecosystem ? ecosystem : `${lang} (${ecosystem})`
}
