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

## For Claude Code: `--claude`

`webctl context --claude` keeps the same fields and replaces `help` with what Claude Code's Bash tool needs to know. It assumes the setup below, where `Bash(webctl:*)` is allowed and the built-in web tools are denied.

```
help[6]:
  A bare webctl command runs under the webctl allow rule. A pipe, 2>&1, redirect, cd or && prefix, loop, or background & puts it before the auto-mode classifier, which can deny it; stderr already reaches the tool result
  Always pass --goal; the relevance filter judges against it
  Output is capped at 20000 chars by default. For less, narrow --goal, then add --summarize (--summarize-model <name> picks its model), then lower --max-output; these replace head, tail, cut, sed, and grep
  fetch reads every URL it is given concurrently; several searches are several bare calls in one message, not background jobs writing files
  Add --scrape --filter-chunks to a search only when you would otherwise read a whole page
  Run `webctl docs <topic>` for the full reference
```

Without it, Claude tends to write `cd <dir> && webctl … 2>&1 | head -40`, or several `webctl … > out.txt 2>&1 &` jobs followed by `head` on the files. An allow rule matches only the bare command, so each of these waits on the auto-mode classifier, and a piped call is sometimes denied. Each line of `help` names the flag that does the shell's job instead. The lines state facts rather than give orders, because Claude Code may show imperative hook context to the user instead of using it.

## As a session-start hook

Claude Code adds a SessionStart hook's output to the conversation, and a SubagentStart hook's `additionalContext` to a subagent's:

```json
{
  "permissions": {
    "allow": ["Bash(webctl:*)"],
    "deny": ["WebSearch", "WebFetch"]
  },
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "webctl context --claude"}]}],
    "SubagentStart": [{"hooks": [{"type": "command", "command": "webctl context --claude | jq -Rs '{hookSpecificOutput: {hookEventName: \"SubagentStart\", additionalContext: .}}'"}]}]
  }
}
```

With no matcher, the SessionStart hook runs after `/clear` and compaction too, so the briefing survives both. Other agents take plain `webctl context`.
