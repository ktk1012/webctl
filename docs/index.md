# Documentation index

Full reference. `--help` is deliberately short; these pages hold the detail. One requirement: a Jev key. Everything else is optional.

| topic | what it covers |
|---|---|
| `search` | the search pipeline, every flag, output formats, exit codes |
| `fetch` | reading pages you already have the URL for, reduced to what the goal needs; no provider, no search quota |
| `providers` | the provider chain, `sources`, keyless vs. keyed, per-provider limits and cost |
| `cooldowns` | rate-limit backoff: the ladder, the probe, the state file, how to tune or clear |
| `filtering` | Jev scoring: the rubric, the threshold, `--noul`, custom rubrics, batch mode |
| `scraping` | `--scrape` and `--filter-chunks`, Reddit handling, fetch fallbacks |
| `summarize` | `--summarize`: a small model condenses each scraped page; command and endpoint backends, model picks, examples |
| `dedupe` | the two duplicate passes and the MinHash/Jev design |
| `config` | every setting, precedence, environment names, the `config` and `keys` commands, file locations |
| `evals` | running and reading the eval suite; the JSON run files it writes |
| `searxng` | running a local SearXNG so searches never hit a quota |

Recommended usage: `webctl search "<query for the engines>" --goal "<what you actually need>"`; both lines reach every judge.

Read one with `webctl docs <topic>`, all of them with `webctl docs all`.
