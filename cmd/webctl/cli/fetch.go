package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dorkitude/webctl/internal/config"
	"github.com/dorkitude/webctl/internal/scrape"
)

// fetchFlags holds the flag values for the fetch command.
type fetchFlags struct {
	goal       string
	maxChars   int
	chunkChars int
	summarize  bool
	sumCommand string
	sumModel   string
	maxOutput  int
	jsonOut    bool
	verbose    bool
}

var ff fetchFlags

func newFetchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fetch [flags] <url>...",
		Short: "Read pages you already have the URL for, keeping only what the goal needs",
		Long: `Fetches each URL, splits it into chunks, and returns only the ones Jev
judges relevant to the goal, so a long article, thread, or PDF costs a
fraction of the tokens reading it whole would.

  webctl fetch https://example.com/post --goal "what you need from it"

--goal is required here. A search has a query for the judges to score
against; this has nothing but the goal, so an empty one leaves them
guessing. Add --summarize when the kept chunks are still longer than you
need, or when you are reading several pages that overlap.

No search runs, so no provider is called: this works with every search
provider cooling down, and costs no search quota.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runFetch(cmd, args)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&ff.goal, "goal", "g", "", "REQUIRED: what you need from these pages; every judge and the summarizer see it")
	f.IntVar(&ff.maxChars, "max-chars", scrape.DefaultMaxChars, "cap text per page")
	f.IntVar(&ff.chunkChars, "chunk-chars", scrape.DefaultChunkChars, "chunk size in characters; judged with 20% overlap")
	f.BoolVar(&ff.summarize, "summarize", false, "replace each page's kept text with a short summary from a small model (see `webctl docs summarize`)")
	f.StringVar(&ff.sumCommand, "summarize-command", "", "summarizer command for this run, e.g. 'claude -p --model haiku' (overrides summarize.command)")
	f.StringVar(&ff.sumModel, "summarize-model", "", "model for the configured summarize.endpoint for this run")
	f.IntVar(&ff.maxOutput, "max-output", DefaultMaxOutput, "cap printed output in characters, trimming page content top-down; 0 = unlimited")
	f.BoolVar(&ff.jsonOut, "json", false, "JSON output")
	f.BoolVarP(&ff.verbose, "verbose", "v", false, "show what each page's filter and summary did")
	return cmd
}

// fetchOptions is the fully-resolved, validated input to the fetch pipeline.
// It embeds searchOptions because every stage it reuses -- scraping, chunk
// filtering, summarizing, output budgeting -- is written against that type.
type fetchOptions struct {
	searchOptions
	URLs []string
}

func runFetch(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	opts, err := resolveFetchOptions(cfg, ff, args)
	if err != nil {
		return err
	}
	return runFetchPipeline(cmd.Context(), cfg, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// resolveFetchOptions merges flags with config defaults and validates them.
func resolveFetchOptions(cfg *config.Config, f fetchFlags, args []string) (fetchOptions, error) {
	urls, err := parseFetchURLs(args)
	if err != nil {
		return fetchOptions{}, err
	}
	goal := strings.TrimSpace(f.goal)
	if goal == "" {
		return fetchOptions{}, errors.New("--goal is required: it is the only thing the chunk filter and the summarizer can judge a page against")
	}
	if f.maxChars <= 0 {
		return fetchOptions{}, fmt.Errorf("--max-chars must be positive, got %d", f.maxChars)
	}
	if f.chunkChars <= 0 {
		return fetchOptions{}, fmt.Errorf("--chunk-chars must be positive, got %d", f.chunkChars)
	}
	if f.maxOutput < 0 {
		return fetchOptions{}, fmt.Errorf("--max-output must be 0 or more, got %d", f.maxOutput)
	}

	opts := fetchOptions{
		searchOptions: searchOptions{
			// The goal stands in for the query. Jev refuses an empty one,
			// and with no search there is nothing else to judge against.
			Query:        goal,
			Goal:         goal,
			Verbose:      f.verbose,
			Scrape:       true,
			MaxChars:     f.maxChars,
			FilterChunks: true,
			ChunkChars:   f.chunkChars,
			MaxOutput:    f.maxOutput,
		},
		URLs: urls,
	}
	if f.jsonOut {
		opts.Format = formatJSON
	}
	if f.summarize || f.sumCommand != "" || f.sumModel != "" {
		sm, err := resolveSummarizer(cfg, f.sumCommand, f.sumModel)
		if err != nil {
			return fetchOptions{}, err
		}
		opts.Summarizer = sm
	}
	return opts, nil
}

// parseFetchURLs validates the arguments as absolute http(s) addresses so a
// typo fails before any network call.
func parseFetchURLs(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		raw := strings.TrimSpace(arg)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a URL: %w", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%q is not an http(s) URL", raw)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%q has no host", raw)
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return nil, errors.New("no URL given")
	}
	return out, nil
}

// runFetchPipeline executes fetch → filter → summarize → output.
func runFetchPipeline(ctx context.Context, cfg *config.Config, opts fetchOptions, out, errOut io.Writer) error {
	// Resolve the Jev key first: the chunk filter runs on every page, so a
	// missing key should fail before anything is fetched.
	jevKey, err := cfg.JevKey()
	if err != nil {
		return err
	}
	// There is no provider excerpt to fall back on and no result title, so
	// both are empty for every URL; a page that cannot be fetched says so.
	blanks := make([]string, len(opts.URLs))
	pages := scrapePages(ctx, opts.searchOptions, newQualifier(cfg, jevKey), opts.URLs, blanks, blanks)
	if opts.Format == formatPretty || opts.Verbose {
		writeScrapeSummary(errOut, pages, opts.searchOptions)
	}
	return writeFetched(out, errOut, opts, pages)
}

// fetchResult is one fetched page, carrying the same content fields a
// search result does without the score and provider ones that do not apply.
type fetchResult struct {
	URL string `json:"url"`

	Page *pageContent `json:"-"`
	// Page fields, filled from Page before encoding.
	Content        string `json:"content,omitempty"`
	ScrapeError    string `json:"scrape_error,omitempty"`
	PDF            *bool  `json:"pdf,omitempty"`
	ChunksTotal    *int   `json:"chunks_total,omitempty"`
	ChunksKept     *int   `json:"chunks_kept,omitempty"`
	ChunksUnjudged *int   `json:"chunks_unjudged,omitempty"`
	FilterError    string `json:"filter_error,omitempty"`
	CharsTrimmed   *int   `json:"chars_trimmed,omitempty"`
	Summarized     *bool  `json:"summarized,omitempty"`
	SummaryError   string `json:"summary_error,omitempty"`
}

func (r *fetchResult) fillPage() {
	if r.Page == nil {
		return
	}
	r.Content, r.ScrapeError, r.ChunksTotal, r.ChunksKept, r.ChunksUnjudged, r.FilterError, r.CharsTrimmed = pageFields(r.Page)
	r.PDF = pdfFlag(r.Page)
	r.Summarized, r.SummaryError = summaryFields(r.Page)
}

// writeFetched prints the fetched pages, trimmed to --max-output.
func writeFetched(w, errOut io.Writer, opts fetchOptions, pages []pageContent) error {
	results := make([]fetchResult, len(pages))
	ptrs := make([]*pageContent, len(pages))
	for i := range pages {
		results[i] = fetchResult{URL: opts.URLs[i], Page: &pages[i]}
		ptrs[i] = &pages[i]
	}
	render := func(i int) string {
		var b strings.Builder
		writeFetchResult(&b, i, results[i])
		return b.String()
	}
	if opts.Format == formatJSON {
		render = func(i int) string {
			results[i].fillPage()
			return marshalIndent(results[i])
		}
	}
	rendered, pagesTrimmed, charsTrimmed := budgetOutput(opts.MaxOutput, ptrs, render)
	writeBudgetSummary(errOut, opts.searchOptions, pagesTrimmed, charsTrimmed)
	if opts.Format == formatJSON {
		for i := range results {
			results[i].fillPage()
		}
		return writeJSON(w, results)
	}
	for _, s := range rendered {
		io.WriteString(w, s)
	}
	return nil
}

func writeFetchResult(w io.Writer, i int, r fetchResult) {
	fmt.Fprintf(w, "[%d] %s\n    %s\n", i+1, hostOf(r.URL), r.URL)
	writeContent(w, r.Page)
	fmt.Fprintln(w)
}
