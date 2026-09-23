# Context

`webctl context` prints a short briefing for an agent's context window, in TOON: the two commands, the rules that matter, and the live state that decides what will work. It reads only local configuration and never touches the network, so it is fast enough to run when a session starts.

```
description: Web search and page reading for agents; Jev keeps only what the goal needs. Prefer it over built-in web search and fetch tools.
version: 0.1.6
jev: configured via TYPESAFE_API_KEY
search: brave, exa
cooling_down: none
summarize: command backend, its own default model
commands[2]{command,use}:
  webctl search <query> --goal <need>,find pages; work from the snippets
  webctl fetch <url>... --goal <need>,read pages you already have the URLs for
help[4]:
  Always pass --goal; the relevance filter judges against it
  Add --scrape --filter-chunks to a search only when you would otherwise read a whole page
  Add --summarize when the kept text is still longer than you need; --summarize-model <name> picks the model for one call
  Run `webctl docs <topic>` for the full reference
```

The state fields are why this is a command rather than a fixed note. A briefing that recommends `--summarize` with no backend configured, or promises a search while every provider is cooling down, sends the agent into errors; this one says what is missing instead:

| field | reports |
|---|---|
| `jev` | whether the key is set, and which variable holds it |
| `search` | the providers a search tries, in order, and whether the keyless ones that throttle are among them |
| `cooling_down` | providers skipped right now, with the time left |
| `summarize` | the backend and model `--summarize` would use, or why it would fail |

`--summarize` is advertised in `help` only when it would work.

## As a session-start hook

Claude Code adds a SessionStart hook's output to the conversation:

```json
{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "webctl context"}]}]}}
```

With no matcher it runs after `/clear` and compaction too, so the briefing survives both.
