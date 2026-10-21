// Minimal Markdown for model-written reports. Everything is HTML-escaped
// first, then a few constructs are turned into tags, so model output can
// never inject markup or script.
function esc(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;')
}

function inline(s: string): string {
  return s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|\W)\*([^*]+)\*(?=\W|$)/g, '$1<em>$2</em>')
}

export function renderMarkdown(src: string): string {
  const lines = esc(src ?? '').split('\n')
  const out: string[] = []
  let list: 'ul' | 'ol' | null = null
  let code = false
  const closeList = () => { if (list) { out.push(`</${list}>`); list = null } }
  for (const raw of lines) {
    const line = raw.trimEnd()
    if (line.startsWith('```')) {
      closeList()
      out.push(code ? '</code></pre>' : '<pre><code>')
      code = !code
      continue
    }
    if (code) { out.push(line); continue }
    let m: RegExpMatchArray | null
    if ((m = line.match(/^(#{1,4})\s+(.*)$/))) {
      closeList()
      const level = Math.min(m[1]!.length + 2, 6)
      out.push(`<h${level}>${inline(m[2]!)}</h${level}>`)
    } else if ((m = line.match(/^\s*[-*]\s+(.*)$/))) {
      if (list !== 'ul') { closeList(); out.push('<ul>'); list = 'ul' }
      out.push(`<li>${inline(m[1]!)}</li>`)
    } else if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) {
      if (list !== 'ol') { closeList(); out.push('<ol>'); list = 'ol' }
      out.push(`<li>${inline(m[1]!)}</li>`)
    } else if (line.trim() === '') {
      closeList()
    } else {
      closeList()
      out.push(`<p>${inline(line)}</p>`)
    }
  }
  closeList()
  if (code) out.push('</code></pre>')
  return out.join('\n')
}
