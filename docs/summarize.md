# Summarize

`--summarize` (with `--scrape`) replaces each scraped page's kept text with a short summary written by a small general model: what the page contributes to the query and goal, facts and figures only, under about 200 words. Jev still decides which chunks are relevant; the summarizer condenses what survived. This is the same shape as the hidden summarization step behind Claude Code's WebFetch, made explicit and pluggable.

Use it when scraped chunks are still too long to read, or when several pages say overlapping things and you want one paragraph each. It costs one small-model call per scraped page (three by default, see `--scrape-top`) and a second or two of latency.

## Backends

Two kinds, chosen by configuration. A command wins when both are set.

**Command.** Any CLI that reads the prompt on stdin and prints the summary on stdout. Run through `sh -c`, so pipes and flags work. A configured model arrives as `$WEBCTL_SUMMARIZE_MODEL`, so one command line can serve several models: `claude -p --model "${WEBCTL_SUMMARIZE_MODEL:-sonnet}"` uses Sonnet unless a model is given.

**Endpoint.** Any OpenAI-compatible `/chat/completions` URL with a model name and a bearer key. `summarize.reasoning_effort` (default `none`) is sent so reasoning models answer instead of thinking; a server that rejects the field gets one retry without it.

The page that has nothing for the goal comes back as "nothing relevant" and prints as an empty summary; a summarizer failure leaves the kept text in place and says so.

## Settings

| setting | env | default | meaning |
|---|---|---|---|
| `summarize.command` | `WEBCTL_SUMMARIZE_COMMAND` | | shell command; prompt on stdin, summary on stdout |
| `summarize.endpoint` | `WEBCTL_SUMMARIZE_ENDPOINT` | | OpenAI-compatible base URL (`.../v1`) or full `/chat/completions` URL |
| `summarize.model` | `WEBCTL_SUMMARIZE_MODEL` | | model name for the endpoint, or `$WEBCTL_SUMMARIZE_MODEL` for the command |
| `summarize.api_key_env` | `WEBCTL_SUMMARIZE_API_KEY_ENV` | by host | env var holding the key: `FIREWORKS_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` are guessed from the URL |
| `summarize.api_key` | `WEBCTL_SUMMARIZE_API_KEY` | | the key itself; prefer the env var |
| `summarize.max_tokens` | `WEBCTL_SUMMARIZE_MAX_TOKENS` | 500 | summary length cap at the endpoint |
| `summarize.reasoning_effort` | `WEBCTL_SUMMARIZE_REASONING_EFFORT` | `none` | sent to the endpoint; empty omits it |
| `summarize.timeout` | `WEBCTL_SUMMARIZE_TIMEOUT` | `60s` | per page |

Per run: `--summarize-command '<cmd>'` and `--summarize-model <name>` override the configured values. A model given for the run selects the endpoint when one is configured; with only a command, the command receives it.

## Recommended models

Small, fast, cheap. The prompt is a few thousand tokens in and a couple of hundred out; a frontier model adds cost and latency for no gain.

| model | how | notes |
|---|---|---|
| DeepSeek V4 Flash | Fireworks endpoint, `accounts/fireworks/models/deepseek-v4p1-flash` | about 1.5 s per page with reasoning off; the default choice for open models |
| Claude Haiku 4.5 | `claude -p --model haiku` command, or Anthropic's OpenAI-compatible endpoint | fast, precise with numbers |
| GPT-5.6 Luna | `codex exec -m gpt-5.6-luna` command, or the OpenAI endpoint | OpenAI's small model |
| GLM 5.3 Flash | Fireworks endpoint, `accounts/fireworks/models/glm-5p3-flash` | alternative open model |
| Kimi K2.6 | Fireworks endpoint, `accounts/fireworks/models/kimi-k2p6` | larger; use when summaries miss detail |

## Examples

Fireworks with DeepSeek Flash (open models; the key comes from `FIREWORKS_API_KEY`):

```bash
webctl config set summarize.endpoint https://api.fireworks.ai/inference/v1
webctl config set summarize.model accounts/fireworks/models/deepseek-v4p1-flash
export FIREWORKS_API_KEY=fw_...
webctl search "kafka min.insync.replicas acks=all" --goal "what happens to produces when the ISR shrinks below the minimum" --scrape --filter-chunks --summarize
```

Fireworks with GLM Flash or Kimi K2.6: same endpoint, different `summarize.model`:

```bash
webctl config set summarize.model accounts/fireworks/models/glm-5p3-flash
webctl config set summarize.model accounts/fireworks/models/kimi-k2p6
```

Claude Haiku through the Claude Code CLI (no key handling; uses your login):

```bash
webctl config set summarize.command 'claude -p --model haiku --no-session-persistence'
```

Each summary started that way is a full Claude Code session: it runs your hooks, reads your CLAUDE.md files and auto-memory, and connects your MCP servers, once per page. `--bare` would skip all of that but reads no OAuth login, so with a subscription the quiet form is spelled out instead. This one also leaves the model to the run, Sonnet by default:

```bash
webctl config set summarize.command 'CLAUDE_CODE_DISABLE_CLAUDE_MDS=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 claude -p --model "${WEBCTL_SUMMARIZE_MODEL:-sonnet}" --no-session-persistence --tools "" --strict-mcp-config --disable-slash-commands --settings '\''{"disableAllHooks":true,"language":"English"}'\'''
webctl fetch <url> --goal "g" --summarize --summarize-model haiku   # one call on Haiku
```

`language` matters only if your settings set a response language: the summary lands in an agent's context, where English costs the fewest tokens.

Claude Haiku through Anthropic's OpenAI-compatible endpoint:

```bash
webctl config set summarize.endpoint https://api.anthropic.com/v1
webctl config set summarize.model claude-haiku-4-5
export ANTHROPIC_API_KEY=sk-ant-...
```

GPT-5.6 Luna through Codex headless:

```bash
webctl config set summarize.command 'codex exec -m gpt-5.6-luna --ephemeral --skip-git-repo-check -s read-only -'
```

GPT-5.6 Luna through the OpenAI endpoint:

```bash
webctl config set summarize.endpoint https://api.openai.com/v1
webctl config set summarize.model gpt-5.6-luna
export OPENAI_API_KEY=sk-...
```

pi with any model it knows (here Kimi K2.6 on Fireworks; pi reads the prompt from a file, so wrap it):

```bash
webctl config set summarize.command 'f=$(mktemp); cat > "$f"; pi -p --no-session --no-tools --no-extensions --no-skills --provider fireworks --model accounts/fireworks/models/kimi-k2p6 "@$f"; rm -f "$f"'
```

A local server (Ollama, LM Studio, vLLM) that speaks the OpenAI API:

```bash
webctl config set summarize.endpoint http://localhost:11434/v1
webctl config set summarize.model qwen3:8b
webctl config set summarize.reasoning_effort ""
```

One-off override without touching config:

```bash
webctl search "q" --goal "g" --scrape --filter-chunks --summarize --summarize-command 'claude -p --model haiku'
webctl search "q" --goal "g" --scrape --filter-chunks --summarize --summarize-model accounts/fireworks/models/kimi-k2p6
```

## Output

Terminal: `--- summary (312 chars from 4,100 kept; 5/11 chunks) ---` per page, or `--- summary: nothing relevant in 2,300 chars of kept text ---`. The scrape line on stderr adds `summarized 3 page(s): 12,400 → 900 chars (4,100 in / 260 out tokens)`. JSON: `content` holds the summary, `summarized: true`, and `summary_error` when the kept text was shown instead.

`--max-output` still applies, though summaries rarely reach it.
