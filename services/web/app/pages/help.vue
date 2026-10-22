<script setup lang="ts">
useHead({ title: 'Help · CodeSynapse' })
const session = useSession()
const toc = [
  { id: 'start', label: 'Getting started' },
  { id: 'terms', label: 'What the numbers mean' },
  { id: 'accounts', label: 'Accounts, organisations and roles' },
  { id: 'git-tokens', label: 'Private repositories' },
  { id: 'memory', label: 'Agent memory' },
  { id: 'privacy', label: 'What leaves your installation' },
]
const terms = [
  { t: 'Tokens', d: 'AI models read and write text in small pieces called tokens; a token is about four characters. Providers charge per token, and a model can only look at so many at once. Fewer tokens means lower cost and more room for the actual task.' },
  { t: 'Tokens avoided', d: 'An estimate of the tokens an agent did not have to read because CodeSynapse answered with a precise excerpt instead of whole files. It compares each answer with reading the files it points to.' },
  { t: 'Security grade (A to F)', d: 'A summary of a repository’s security findings, weighted by severity and by whether the code is reachable from outside. It needs the repository’s files to be covered by the checks; without that it shows N/A rather than a misleading A.' },
  { t: 'Finding', d: 'A concrete problem in code or configuration, such as a command built from user input or a Kubernetes container running as root. Every finding names the rule that flagged it and explains the fix.' },
  { t: 'Vulnerability (reachable)', d: 'A known security flaw (CVE) in a dependency. “Called” means your code calls the affected function, which matters most; “imported” and “required” are progressively less likely to affect you.' },
  { t: 'Licence conflict', d: 'A dependency whose licence does not fit your project’s licence, for example a strong copyleft licence (GPL) in a proprietary or Apache-2.0 product. Conflicts can force you to publish code or replace the dependency, so they are worth fixing early.' },
  { t: 'Unused code', d: 'Functions, packages, modules and configuration nothing reaches from the program’s entry points. Removing it shrinks what you maintain, audit and pay agents to read.' },
  { t: 'Review', d: 'Comments on a change, written by an agent or by CodeSynapse’s AI tasks, anchored to file and line, each with a suggestion you can accept, reject or turn into a fix branch.' },
  { t: 'Latency, p50 and p95', d: 'How long answers take. p50 is the typical answer; p95 is the slow end (95 out of 100 are faster). Agents wait on every call, so p95 is the one users feel.' },
]
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Help" title="How CodeSynapse works" lede="CodeSynapse reads your repositories once and keeps a map of them: code, configuration, documentation, history and dependencies. People use it to see what needs attention; AI agents use it to read only what they need." />
    <nav class="toc panel" aria-label="On this page">
      <a v-for="s in toc" :key="s.id" :href="`#${s.id}`">{{ s.label }}</a>
    </nav>

    <section id="start" class="panel">
      <h2>Getting started</h2>
      <ol class="steps">
        <li><strong>Add a repository.</strong> On <NuxtLink to="/">Repositories</NuxtLink>, paste a Git URL. Public repositories need nothing else; private ones need a <a href="#git-tokens">Git token</a>. Indexing takes from seconds to a few minutes.</li>
        <li><strong>See what needs attention.</strong> <NuxtLink to="/overview">Overview</NuxtLink> lists the most urgent security findings, vulnerable dependencies you actually call, licence conflicts and repositories that are out of date.</li>
        <li><strong>Connect your AI agent.</strong> <NuxtLink to="/agents">Agents</NuxtLink> walks you through it in three steps.</li>
        <li><strong>Keep it current.</strong> Each repository can sync on a schedule, and reviews and security triage can run after every sync.</li>
      </ol>
    </section>

    <section id="terms" class="panel">
      <h2>What the numbers mean</h2>
      <dl class="terms">
        <template v-for="x in terms" :key="x.t"><dt>{{ x.t }}</dt><dd>{{ x.d }}</dd></template>
      </dl>
    </section>

    <section id="accounts" class="panel">
      <h2>Accounts, organisations and roles</h2>
      <p>Everything belongs to an <strong>organisation</strong>: its repositories, agent memory, reviews and statistics. Members of one organisation never see another’s. The same public repository is indexed once and shared, so adding it is instant if someone already has.</p>
      <ul>
        <li><strong>Viewer</strong> reads repositories, reports and statistics.</li>
        <li><strong>Member</strong> also adds and syncs repositories and runs AI tasks.</li>
        <li><strong>Admin</strong> also manages members, Git tokens and settings.</li>
        <li><strong>Owner</strong> can do everything, including managing other owners.</li>
      </ul>
      <p>The first account of an installation is its <strong>platform administrator</strong>, who sees every organisation and the audit log. Every change and sign-in is recorded in a tamper-evident audit log, which is what NIS2-regulated organisations need.</p>
      <p v-if="!session.accounts.value" class="notice">This installation runs without accounts. To turn them on, configure PostgreSQL (<code>CODESYNAPSE_DATABASE_URL</code>); the docker compose setup does this for you.</p>
    </section>

    <section id="git-tokens" class="panel">
      <h2>Private repositories</h2>
      <p>An organisation admin stores a <strong>read-only</strong> token for each Git host under <NuxtLink to="/settings/git-tokens">Settings → Git tokens</NuxtLink>: a GitHub fine-grained token with “Contents: read”, or a GitLab project or group token with “read_repository”. CodeSynapse encrypts it and uses it only to clone and sync your organisation’s repositories.</p>
      <p>Your organisation sees a private repository only while its own token can read it. CodeSynapse checks at every sync; if the token was revoked or lost access, the repository is hidden from you until a working token is saved. Another organisation never gets your repositories through your token.</p>
      <p class="muted small">For whoever runs CodeSynapse: tokens are encrypted with <code>CODESYNAPSE_SECRET_KEY</code> (<code>just init</code> creates it). Keep it safe; tokens encrypted with a lost key have to be entered again.</p>
    </section>

    <section id="memory" class="panel">
      <h2>Agent memory</h2>
      <p>Agents forget everything between sessions. CodeSynapse gives them a shared memory per repository: decisions, conventions, who owns what and open issues, saved as short facts. A new session can recall what earlier sessions learned instead of rediscovering it, which saves both time and tokens.</p>
      <p>Facts are notes, not instructions: agents are told to check them against the code before acting. Each organisation’s memory is separate, even for a repository you share with others.</p>
    </section>

    <section id="privacy" class="panel">
      <h2>What leaves your installation</h2>
      <ul>
        <li>Source code stays on your installation. The map stores names, signatures, line ranges and short summaries, never whole files or secret values.</li>
        <li>Dependency names and versions are looked up at osv.dev (vulnerabilities) and deps.dev (licences).</li>
        <li>AI tasks send text to the model provider you configured. Cloud models never receive source code unless you allow it, and conversation memory is extracted only by local models.</li>
      </ul>
    </section>
  </div>
</template>

<style scoped>
.toc { flex-direction: row; flex-wrap: wrap; gap: 6px 18px; padding: 14px 20px; }
.steps { margin: 0; padding-left: 20px; display: grid; gap: 8px; }
.terms { display: grid; grid-template-columns: minmax(160px, 220px) 1fr; gap: 10px 20px; margin: 0; }
.terms dt { font-weight: 600; }
.terms dd { margin: 0; color: var(--muted); }
section[id] { scroll-margin-top: 16px; }
section ul { display: grid; gap: 6px; }
@media (max-width: 640px) { .terms { grid-template-columns: 1fr; } .terms dd { margin-bottom: 8px; } }
</style>
