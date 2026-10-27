# Local models

!!! info "In plain words"
    A **local model** is an AI that runs on your own computer instead of in someone else's cloud. It is slower, but it costs nothing per question and your code never leaves your machine. CodeSynapse uses it to read files for your coding assistant and hand back a short answer, so the assistant spends fewer paid tokens. A **token** is the unit AI services charge by: roughly three quarters of a word. See the [glossary](../GLOSSARY.md#ai-assistants).

A model on your own machine does the reading and writing that would otherwise go through a paid cloud model. CodeSynapse routes every AI task to it, and coding agents hand it file reading through `ask_local`. Nothing a local model reads leaves your machine or network.

This page covers why you would run one, what the two recommended Qwen models need, and how to set them up.

## What a local model does here

| Work | Task | Local only? |
|---|---|---|
| Agents ask it about files instead of reading them (`ask_local`) | `local-answer` | yes |
| It briefs the agent on each code prompt before the agent starts (the plugin's pre-brief) | `local-answer` | yes |
| Older chat turns become memory facts before a compaction | `memory` | yes |
| Recall by meaning (embeddings) | `embeddings` | yes, with `embedding_model` |
| Change summaries, cleanup briefings | `pr-summary`, `cleanup-briefing` | yes |
| Code review, security triage, briefings | `review`, `security-triage`, `briefing` | no, but a local model may see source |
| Fix branches for accepted review comments | `fix` | no, but it needs source, so use a local one |
| Symbol annotations | `code-annotation` | yes |

"Local" means the provider's `base_url` is on this machine or a private network (`localhost`, `host.docker.internal`, `*.local`, private addresses). The local-only tasks never fall back to a cloud model. Without a local model they are unavailable, and `ask_local` tells the agent to read the files itself.

## Why run one

**Fewer cloud tokens.** A coding agent pays for every line it reads. Asked how a task picks its provider, `ask_local` read the 718-line `llm.go` (about 10,600 tokens) on `qwen3.8:27b` and gave the agent a correct, cited answer of about 700 tokens. The agent only reads the few lines it cites when it needs the detail.

**No per-token price for AI tasks.** Reviews, triage, briefings and memory extraction cost electricity, not tokens, so you can run them on every sync instead of rationing them.

**Source stays with you.** Cloud providers get the diff you ask them to review and nothing else. Local providers may also get source snippets, which is why the review and fix tasks work better on one, and why `ask_local`, memory and summaries run only locally.

**It works offline** once the models are pulled.

## What it costs you

**Memory.** The model has to sit in RAM (unified memory on a Mac, VRAM on a GPU) next to the stack, your editor and the agent. A model that does not fit is swapped in and out and becomes unusably slow.

**Speed.** A local model is slower than a cloud one, sometimes by minutes. CodeSynapse is set up for that: timeouts scale with the prompt, `ask_local` waits up to 30 minutes and splits large inputs into batches. When saving tokens matters more than waiting, that trade is worth it.

**Quality.** A 27B model is good at reading code and answering with references; it is weaker than a frontier cloud model at long chains of reasoning. `ask_local` checks every `file:line` citation against the lines it sent and marks answers that cite nothing (`grounded: false`), and agents are told to read the cited lines before editing.

**Upkeep.** Someone pulls models, keeps the server running and sets its context length. Model licences vary (see below).

## The two Qwen models

Qwen is a family of open AI models you can download. Two words help with the table: *parameters* are the model's learned numbers (more usually means smarter and bigger), and the *context* is how much text the model can hold in mind at once, counted in tokens.

| | `qwen3.8:27b` | Qwen3.8-Flash-Next (125B) |
|---|---|---|
| Design | Dense, 27.8B parameters | Mixture of experts: 125B total, about 6B active per token |
| Context | 262,144 tokens | 262,144 tokens |
| Ollama tags | `qwen3.8:27b` (18 GB, nvfp4 on Apple Silicon), `27b-q4_K_M` 18 GB, `27b-q8_0` 30 GB, `27b-bf16` 56 GB | `qwen3.8-flash-next:125b-mlx` 105 GB (Apple Silicon only), `125b-a6b-q4_K_M` 120 GB |
| Memory to run it | 18 GB for the model; 32 GB machine minimum, 48 GB comfortable with the stack and an agent | 96 GB minimum for 1- to 3-bit builds, 128 GB for the 4-bit ones |
| Speed | M5 Pro, 48 GB, 32k context: about 440 tokens/s reading the prompt, 31 tokens/s writing (measured for this page) | 48 to 74 tokens/s writing on an M5 Max with 128 GB, by runtime (third-party figures) |
| Licence | Apache 2.0 (`ollama show`) | Qwen Community License, not Apache 2.0 (third-party report; check before use) |
| Use it when | You have 32 to 64 GB. It is the default recommendation. | You have 128 GB or more and want answers closer to cloud quality. Fewer active parameters make it fast for its size, but all 125B must be in memory. |

Measured on an Apple M5 Pro with 48 GB: `qwen3.8:27b` used 17.5 to 18.2 GiB with a 32k context and loaded in under 10 seconds. Long contexts add a key-value cache on top; keep 6 GB or more free if you run it at the full 256k. The Flash-Next figures come from the sources at the end of this page, not from our own runs.

Smaller machines can use `qwen3:14b` (8.6 GB) or `qwen3:8b` (4.9 GB). Answers get shorter on detail and citations get less precise, so `ask_local` will more often say it found nothing and the agent reads the files itself.

Every setup also needs the embedding model `nomic-embed-text` (0.3 GB) for recall by meaning.

The CodeSynapse stack itself uses about 1 GB across its containers at rest (measured with `podman stats`); see [Requirements](requirements.md).

## Set it up with Ollama

[Ollama](https://ollama.com) is a free program that downloads models and runs them on your machine. These steps assume you have the CodeSynapse stack running (see [Running the stack](running.md)).

1. Install [Ollama](https://ollama.com) and pull the models:

    ```sh
    ollama pull qwen3.8:27b
    ollama pull nomic-embed-text
    ```

2. Raise the context length. The Ollama app has it under Settings (set it to 256k); a server started from the shell reads `OLLAMA_CONTEXT_LENGTH=262144`. CodeSynapse cannot set it through Ollama's OpenAI-compatible API.

3. Let CodeSynapse pick, or choose yourself.

    **Automatic.** With no provider in `config/llm.yaml`, the API looks for Ollama (port 11434) or LM Studio (1234) when it starts and uses it. Or add one with `model: auto`:

    ```yaml
    providers:
      - name: local
        kind: openai
        base_url: http://host.docker.internal:11434/v1
        model: auto            # the chat model with the most parameters
        embedding_model: auto  # an embedding model, for recall by meaning
    ```

    CodeSynapse asks the server what it serves at start-up and every five minutes. It picks the chat model with the most parameters, since answer quality matters more than speed, uses that model's own context length as `context_window` and, for a window of 64k or more, leaves 32k tokens for the answer (thinking models reason in it). A model you pull later is used within five minutes, without a restart; if the server stops answering, the last choice stays. The AI models page lists what the server offers and what was picked. `auto` works only for local servers. Set `CODESYNAPSE_LLM_DISCOVER=false` to turn off discovery, or `CODESYNAPSE_LLM_DISCOVER_URLS` to list other addresses.

    **By hand.** Name the model and its settings:

    ```yaml
    providers:
      - name: local-ollama
        kind: openai
        base_url: http://host.docker.internal:11434/v1   # the compose containers reach the Mac this way
        model: qwen3.8:27b
        embedding_model: nomic-embed-text
        context_window: 262144
        max_output_tokens: 32768
        effort: none              # Ollama turns thinking off with this; it ignores disable_thinking
        bytes_per_token: 3        # fits about three times more into each request than the safe default
        input_budget_policy: estimate
    default: local-ollama
    ```

    For Flash-Next, change `model` to `qwen3.8-flash-next:125b-mlx` (Apple Silicon) or `qwen3.8-flash-next:125b-a6b-q4_K_M`.

4. Restart the API and the workers so they read the file: `podman compose up -d api worker fix-worker` (or `docker compose`).

5. Open **AI models** in the dashboard. The provider should say Online. A model the server does not have is named with the `ollama pull` command to fix it, and the API logs the same at start-up.

6. Leave every task on "Use global". With a local global model everything runs locally, and when you change the global model every task follows it. Pin a task to another model only when you want a different one there.

7. Connect your agents with the [Claude Code plugin](claude-code-plugin.md) or `codesynapse mcp install`. `ask_local` is in the core tools, and the session rules, server instructions and hooks point agents at it, so they use it without being asked. The bridge part needs a `codesynapse` release newer than 0.0.13.

To check it works, ask an agent a question about a large file and look for an `ask_local` call. Statistics → Agents shows the local models' work under Local models.

## Queueing and shorter prompts

A local provider answers one request at a time (`max_concurrent`, default 1). If several agents ask at once, the rest wait in a queue and continue when their answer is ready. That keeps the GPU busy without overloading it. A waiting request gives up only when its caller does. Raise `max_concurrent` if your server batches requests (for Ollama set `OLLAMA_NUM_PARALLEL` too).

With `compress_prompts: true` at the top of `config/llm.yaml`, a prompt bound for a cloud provider first goes through a local model (task `compress`, local only) that rewrites its prose in short telegraphic form. Code, diffs, lists and JSON are never touched, a rewrite that drops a number or a backticked name is discarded, and without a local model the original is sent. The cloud model is also asked for terse answers. Bytes removed show as `compressed_bytes` in the provider usage.

## Other servers

Anything with an OpenAI-compatible `/v1/chat/completions` works with `kind: openai`: MLX (`mlx_lm.server`), LM Studio, vLLM, llama.cpp. On MLX, vLLM and SGLang use `disable_thinking: true` instead of `effort: none`. See [AI models](running.md#ai-models) for every provider setting.

## Sources

- [Ollama library: qwen3.8 tags](https://ollama.com/library/qwen3.8/tags)
- [Unsloth: Qwen3.8-Flash-Next, how to run locally](https://unsloth.ai/docs/models/qwen3.8-next) (memory per quantization)
- [modelfit.io: Qwen3.8-Flash-Next specs and Mac speeds](https://modelfit.io/blog/qwen38-flash-next-open-weights/) (active parameters, Ollama tags, licence, decode speeds)
