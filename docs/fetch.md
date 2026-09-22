# Fetch

`webctl fetch <url>...` reads pages you already have the address for. Each
page is fetched, split into chunks, and reduced to the chunks Jev judges
relevant to the goal, so a long article, forum thread, or PDF costs a
fraction of the tokens reading it whole would.

```bash
webctl fetch https://kafka.apache.org/documentation/#producerconfigs \
  --goal "what acks=all does when the ISR shrinks below min.insync.replicas"
```

This is the same reduction `search --scrape --filter-chunks` performs on the
results it finds, offered on its own for the case where finding is not the
problem: a link from a colleague, an error message, a changelog, a page you
already know you need.

## `--goal` is required

A search has a query for the judges to score chunks against. This has
nothing else, so the goal is the whole basis of the judgment and an empty
one leaves it guessing. It is the single biggest lever on what comes back.

## No provider is called

Fetch runs no search, so it consumes no search quota, needs no search key,
and works while every provider is cooling down. Only the Jev key is
required, plus a summarizer backend if you pass `--summarize`.

## Flags

| flag | default | meaning |
|---|---|---|
| `--goal`, `-g` | | REQUIRED: what you need from these pages |
| `--summarize` | off | replace each page's kept text with a short summary from a small model; see `summarize` |
| `--summarize-command` | | summarizer command for this run, e.g. `'claude -p --model haiku'` |
| `--summarize-model` | | model for the configured `summarize.endpoint` for this run |
| `--max-chars` | 50000 | cap text per page before chunking |
| `--chunk-chars` | 2000 | chunk size in characters; judged with 20% overlap |
| `--max-output` | 20000 | cap printed output in characters, trimming content top-down; 0 = unlimited |
| `--json` | off | JSON array on stdout; diagnostics stay on stderr |
| `--verbose`, `-v` | off | show what each page's filter and summary did |

Several URLs may be given at once; they are fetched concurrently and printed
in the order you listed them.

## Reach for `--summarize` when

the kept chunks are still longer than you need, or you are reading several
pages that overlap. It costs one small-model call per page and replaces the
kept text with a paragraph of facts and figures. A page with nothing for the
goal comes back empty rather than padded.

## Output

Pretty output prints one block per URL: the host, the address, then the
content under a `--- content (k/n chunks kept, c chars) ---` header, or
`--- summary (...) ---` with `--summarize`. A page that could not be fetched
says so and the rest still print.

JSON gives an array of objects with `url` and `content`, plus `chunks_total`,
`chunks_kept`, `chunks_unjudged`, `pdf`, `chars_trimmed`, `summarized`, and
the `scrape_error`, `filter_error`, `summary_error` fields when something
went wrong.

PDFs are detected and their text layer is chunk-filtered like any other page.
