# Glossary

Plain-English meanings for the words you will meet in CodeSynapse. Each entry says what the word means here, not in general.

## The basics

**Repository**
:   A project's folder of code, tracked by git. CodeSynapse reads repositories. It never changes your code, except when you ask it to prepare a fix branch (see *Fix branch*).

**Index / ingest**
:   Reading a repository once and writing down what is in it: functions, who calls whom, documents, dependencies, history. "Ingest" and "index" mean the same thing here. Run it again after the code changes (or let the scheduled sync do it).

**Graph**
:   A way to store facts as dots and lines. A dot is a thing (a function, a file, a package). A line is a link between two things ("this function calls that one"). Asking "who calls this function?" means following the lines. CodeSynapse stores the graph in NebulaGraph, a database made for this.

**Symbol**
:   A named piece of code: a function, a method, a type. When the docs say "ask for a symbol", they mean "ask about one named function or type".

**Caller / callee**
:   If function A calls function B, then A is the *caller* and B is the *callee*.

**Dependency**
:   Code from someone else that your project uses, such as a library. CodeSynapse lists them with their versions and licences.

**CVE**
:   A public ID for a known security hole in a piece of software (for example `CVE-2024-12345`). CodeSynapse checks your dependencies against a public list of them (osv.dev) and tells you which ones affect you. Only package names and versions are sent for that check, never your code.

**Reachability**
:   Whether your code can actually run the vulnerable part of a dependency. A hole in a library you import but never call is less urgent than one you call directly. Findings say which case applies.

## AI assistants

**Coding agent (or just "agent")**
:   An AI assistant that works on code for you, such as Claude Code, Codex or Cursor. It reads files, runs commands and edits code.

**Token**
:   The unit an AI model reads and writes. Roughly three-quarters of a word. Models limit how many tokens they can read at once, and cloud models charge per token. Fewer tokens means faster, cheaper and more focused answers.

**Context**
:   Everything the model is shown for one question: your message, the files it read, earlier turns. Large context costs more and can bury the useful part.

**MCP (Model Context Protocol)**
:   A standard way for an AI assistant to call tools that live outside it. CodeSynapse runs an *MCP server*: your agent connects to it and can then call tools such as "show me this function and its callers". These calls only read, apart from the few documented review and memory tools.

**Tool**
:   One thing an agent can ask CodeSynapse to do, such as `search` or `get_symbol_context`. The [Agent guide](AGENT_GUIDE.md) lists them.

**Local model**
:   An AI model that runs on your own machine (for example through Ollama) instead of a cloud service. Your text does not leave the machine. CodeSynapse uses one for jobs that touch source code or conversations, such as shortening text before it goes to a cloud model.

**Gateway**
:   An optional front door for your agent's own model traffic. Instead of your agent talking to the model provider directly, it talks to CodeSynapse first. CodeSynapse puts the request in a queue, offers it to your local model first, and forwards it to the cloud model when the local one cannot answer it well. It is on by default; the operator or the organisation can switch it off. See the [gateway plan](plans/GATEWAY_PLAN.md).

**Queue**
:   A waiting line. A local model can only do so much at once, so requests wait their turn.

**Embedding / vector**
:   A list of numbers that stands for the meaning of a short text. Texts with similar meaning get similar numbers, which lets CodeSynapse find related memory by meaning. The numbers live in Milvus. Only entity names are embedded, by a local model.

**Agent memory**
:   Facts an agent chooses to save ("this service owns billing") so a later chat can look them up. Recalled facts are data to check, not orders to follow. See [Agent memory](features/memory.md).

## Running CodeSynapse

**Stack**
:   The set of programs that make up CodeSynapse: the API, the worker, the MCP server, the web dashboard and their databases. `just up` starts all of them.

**API**
:   The service the dashboard talks to. It also records background jobs.

**Worker**
:   A background program that does the slow jobs: cloning, indexing, running AI tasks. A separate *fix worker* is the only one allowed to push code.

**Dashboard**
:   The web interface at `http://localhost:3000`.

**Job**
:   One piece of background work, like "index this repository". Jobs survive restarts.

**Sync**
:   Fetching new commits for a repository and indexing them again.

**Compose**
:   Docker Compose or podman compose: a tool that starts several containers together from one file (`docker-compose.yml`).

**Container**
:   A packaged program that runs in isolation, so it behaves the same on every machine.

**Helm chart**
:   The package for installing CodeSynapse on Kubernetes.

**Kubernetes**
:   A system for running containers across many machines. Optional; a laptop with compose is enough to start.

## People and access

**Organisation**
:   A group of people and repositories that share one space. One organisation cannot see another's data.

**Role**
:   What a person may do in an organisation: owner, admin, member, viewer or guest. Organisation roles manage people and settings; access to code is granted per repository.

**API key**
:   A long secret (starting with `csk_`) that lets a program, such as an agent, act as you. It works in one organisation only and stops working if you are removed. It is shown once when you create it.

**SSO (single sign-on)**
:   Signing in with your company login instead of a separate password. CodeSynapse supports OpenID Connect for this, set up per organisation.

**Root**
:   The first administrator account of an installation. It manages the platform but cannot read any organisation's code.

**Runtime session**
:   A short-lived identity an agent registers so each of its requests can be traced to a person and a chat. The [session context plan](plans/SESSION_CONTEXT_PLAN.md) has the details.

**Audit log**
:   A tamper-evident record of who did what and when. Every change and security event is in it.

## Security review

**Finding**
:   One problem a security rule found, tied to the function that owns it.

**Rule**
:   A check for one kind of problem, kept as a file in `rules/`.

**Grade (A–F)**
:   A summary score for a repository's security findings.

**SBOM**
:   Software bill of materials: a list of every component in your software, in a standard format.

**Fix branch**
:   A new git branch with a suggested fix for a review comment you accepted. It opens a *draft* pull request for a person to review. It is off unless an operator enables pushing, and it never force-pushes.

## Operations

**Hook**
:   A small program an assistant such as Claude Code runs by itself at a set moment, for example when a chat starts or before a file is read.

**Plugin**
:   An add-on for Claude Code. The CodeSynapse plugin bundles the connection to the server, a usage guide and the hooks in one install.

**Bridge**
:   A small program on your computer (`codesynapse mcp bridge`) between Claude Code and the CodeSynapse server. It adds who you are and which chat it is to every request.

**Migration**
:   An automatic update of the database layout. It runs when a new version starts, so you do not do it by hand.

**Lease**
:   A temporary claim on a task. One API process holds it at a time, so a task is not done twice when several run. If it stops, another takes over.

**HA (high availability)**
:   A setup that keeps working when one machine fails, because every part has a spare.

**Failover**
:   The moment a spare takes over from a part that failed.

**Session memory**
:   Short notes about one chat, kept so old messages can leave the chat and still be found. They expire after seven days.

**Redaction**
:   Removing secrets (passwords, keys) and personal data from text before it is stored or sent.

**Tag (release)**
:   A name pinned to one commit, such as `v0.3.0`. Pushing a tag starts the release build.

**Cosign**
:   A tool that signs container images and charts, so you can check they came from the project.

## More terms

**Tenant**
:   One customer of a shared installation. In CodeSynapse a tenant is an organisation: its repositories, reviews and memory stay private to it.

**OpenID Connect (OIDC)**
:   The standard sign-in protocol behind single sign-on. Okta, Microsoft Entra ID, Keycloak and Google Workspace all speak it.

**Telemetry**
:   Numbers about how the system behaves (requests, delays, errors). CodeSynapse never puts prompts, code or file paths into them.

**NebulaGraph**
:   The database that stores the code graph.

**Milvus**
:   The database that stores vectors for agent memory.

**PostgreSQL**
:   The database that stores accounts, organisations, sessions and the audit log.

**Node / edge**
:   The "dots" and "lines" of the graph. A node is a thing (a function, a file); an edge is a link between two nodes (for example "calls").
