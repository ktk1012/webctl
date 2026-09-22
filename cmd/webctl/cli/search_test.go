package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dorkitude/webctl/internal/config"
	"github.com/dorkitude/webctl/internal/jev"
	"github.com/dorkitude/webctl/internal/keys"
	"github.com/dorkitude/webctl/internal/provider"
	"github.com/dorkitude/webctl/internal/scrape"
	"github.com/dorkitude/webctl/internal/summarize"
)

// fakeProvider records the search it was asked to run and returns canned results.
type fakeProvider struct {
	name    string
	results []provider.SearchResult
	err     error
	// hang makes Search block until its context is done.
	hang bool

	gotQuery string
	gotNum   int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Search(ctx context.Context, query string, num int) ([]provider.SearchResult, error) {
	f.gotQuery, f.gotNum = query, num
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.results, f.err
}
func (f *fakeProvider) Validate(context.Context) error { return nil }

// fakeQualifier returns a score (or noul probability) per URL.
type fakeQualifier struct {
	scores map[string]float64 // URL → score / P(yes)
	errs   map[string]error   // URL → per-result error
	err    error              // whole-request error
	dupes  map[string]bool    // "urlA|urlB" → confirmed duplicate

	gotQuery string
	gotGoal  string
	gotOpts  jev.QualifyOptions
	calls    int

	// Chunk filtering: chunks containing a relevantWord are "yes"; a chunk
	// containing unansweredWord gets no answer; chunkErr fails the request.
	relevantWord   string
	unansweredWord string
	chunkErr       error
	// chunkUnjudged, when set, returns a *jev.PartialFilterError naming these
	// chunk indices, as a page whose batches partly failed would.
	chunkUnjudged []int
	mu            sync.Mutex
	chunkCalls    int
	gotChunks     [][]jev.Chunk
}

// dupes maps "urlA|urlB" to whether the fake confirms them as duplicates.
func (f *fakeQualifier) ConfirmDuplicates(_ context.Context, _ jev.Ask, results []provider.SearchResult, pairs []jev.DuplicatePair) ([]bool, jev.Usage, error) {
	out := make([]bool, len(pairs))
	for i, p := range pairs {
		out[i] = f.dupes[results[p.A].URL+"|"+results[p.B].URL] || f.dupes[results[p.B].URL+"|"+results[p.A].URL]
	}
	return out, jev.Usage{InputTokens: 7}, nil
}

func (f *fakeQualifier) FilterChunks(_ context.Context, ask jev.Ask, chunks []jev.Chunk) ([]*jev.NoulAnswer, jev.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunkCalls++
	f.gotChunks = append(f.gotChunks, chunks)
	if f.chunkErr != nil {
		return nil, jev.Usage{}, f.chunkErr
	}
	out := make([]*jev.NoulAnswer, len(chunks))
	for i, c := range chunks {
		lc := strings.ToLower(c.Text) // judged on the chunk alone, as the prompt instructs
		if f.unansweredWord != "" && strings.Contains(lc, strings.ToLower(f.unansweredWord)) {
			continue
		}
		p := 0.1
		if f.relevantWord != "" && strings.Contains(lc, strings.ToLower(f.relevantWord)) {
			p = 0.9
		}
		out[i] = &jev.NoulAnswer{Probability: p}
	}
	if len(f.chunkUnjudged) > 0 {
		for _, i := range f.chunkUnjudged {
			if i < len(out) {
				out[i] = nil
			}
		}
		return out, jev.Usage{}, &jev.PartialFilterError{
			Unjudged: f.chunkUnjudged, Batches: 2, Failed: 1, Err: errors.New("jev down"),
		}
	}
	return out, jev.Usage{}, nil
}

func (f *fakeQualifier) Qualify(_ context.Context, ask jev.Ask, results []provider.SearchResult, opts jev.QualifyOptions) ([]jev.Qualified, jev.Usage, error) {
	f.calls++
	f.gotQuery, f.gotOpts, f.gotGoal = ask.Query, opts, ask.Goal
	if f.err != nil {
		return nil, jev.Usage{}, f.err
	}
	out := make([]jev.Qualified, len(results))
	for i, r := range results {
		out[i].Result = r
		if err, ok := f.errs[r.URL]; ok {
			out[i].Err = err
			continue
		}
		val := f.scores[r.URL]
		if opts.Noul != "" {
			out[i].Noul = &jev.NoulAnswer{Probability: val}
			continue
		}
		max := 3
		if len(opts.Rubric) > 0 {
			max = len(opts.Rubric) - 1
		}
		probs := map[string]float64{}
		for lvl := 0; lvl <= max; lvl++ {
			probs[itoa(lvl)] = 0.05
		}
		probs[itoa(int(val+0.5))] = 0.8
		out[i].Score = &jev.ScoreAnswer{Score: val, Confidence: 0.8, Probabilities: probs}
	}
	return out, jev.Usage{InputTokens: 100 * len(results), OutputTokens: len(results)}, nil
}

func itoa(i int) string { return string(rune('0' + i)) }

var (
	paper = provider.SearchResult{Title: "Attention Is All You Need", URL: "https://arxiv.org/abs/1706.03762", Snippet: "We propose the Transformer."}
	blog  = provider.SearchResult{Title: "SEO blog", URL: "https://www.content-farm.example/attention", Snippet: "Top 10 attention tips."}
	wiki  = provider.SearchResult{Title: "Transformer (deep learning)", URL: "https://en.wikipedia.org/wiki/Transformer", Snippet: "A transformer is a deep learning architecture."}
)

// harness wires fakes into the CLI and runs it against a temp config dir.
type harness struct {
	t    *testing.T
	dir  string
	prov *fakeProvider
	qual *fakeQualifier
	// provs, when set, supplies a distinct fake per provider name; names not
	// present fall back to prov.
	provs map[string]*fakeProvider
	// built records the provider names constructed, in order.
	built []string
}

func newHarness(t *testing.T, store keys.Store) *harness {
	t.Helper()
	for _, n := range keys.All {
		for _, env := range n.EnvVars() {
			t.Setenv(env, "")
		}
	}
	for _, k := range []string{"PROVIDER", "NUM", "MIN_SCORE", "MIN_RESULTS", "JEV_BASE_URL", "JEV_MODEL"} {
		t.Setenv(config.EnvPrefix+"_"+k, "")
	}
	dir := t.TempDir()
	if err := store.Save(filepath.Join(dir, "keys.json")); err != nil {
		t.Fatal(err)
	}
	// Fakes answer with three results, which would trigger a top-up on
	// every search; the dedicated test turns it back on.
	chainTopUp = false
	t.Cleanup(func() { chainTopUp = true })
	t.Setenv(config.EnvPrefix+"_SOURCES", "1")
	origCooldown := newCooldown
	newCooldown = func(cfg *config.Config) provider.Cooldowns { return provider.NewCooldown("", cfg.Cooldown) }
	t.Cleanup(func() { newCooldown = origCooldown })
	h := &harness{
		t:    t,
		dir:  dir,
		prov: &fakeProvider{name: "exa", results: []provider.SearchResult{blog, paper, wiki}},
		qual: &fakeQualifier{scores: map[string]float64{paper.URL: 2.9, wiki.URL: 2.4, blog.URL: 0.3}},
	}
	origProv, origQual := newProvider, newQualifier
	newProvider = func(cfg *config.Config, name string) (provider.Provider, error) {
		if _, err := cfg.ProviderKey(name); err != nil {
			return nil, err
		}
		h.built = append(h.built, name)
		if p, ok := h.provs[name]; ok {
			p.name = name
			return p, nil
		}
		h.prov.name = name
		return h.prov, nil
	}
	newQualifier = func(cfg *config.Config, key string) qualifier {
		if key == "" {
			t.Error("qualifier constructed with empty key")
		}
		return h.qual
	}
	t.Cleanup(func() { newProvider, newQualifier = origProv, origQual })
	return h
}

// run executes the CLI with args, returning stdout, stderr, and the error.
func (h *harness) run(args ...string) (string, string, error) {
	h.t.Helper()
	v = config.New()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--config-dir", h.dir, "--keys-file", filepath.Join(h.dir, "keys.json")}, args...))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func allKeys() keys.Store {
	return keys.Store{ExaAPIKey: "e", ParallelAPIKey: "p", SonarAPIKey: "s", JevAPIKey: "j"}
}

func mustJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return v
}

func TestSearchDefaultFiltersAndSorts(t *testing.T) {
	h := newHarness(t, allKeys())
	out, errOut, err := h.run("transformer", "circuits")
	if err != nil {
		t.Fatal(err)
	}
	if h.prov.gotQuery != "transformer circuits" || h.prov.gotNum != config.DefaultNum {
		t.Errorf("provider got %q / %d", h.prov.gotQuery, h.prov.gotNum)
	}
	if h.qual.gotQuery != "transformer circuits" || h.qual.gotOpts.Batch || h.qual.gotOpts.Noul != "" || h.qual.gotOpts.Rubric != nil {
		t.Errorf("qualifier opts = %+v", h.qual.gotOpts)
	}
	// Kept results are sorted by score, highest first; the blog is dropped.
	if !strings.Contains(out, "[1] Attention Is All You Need — arxiv.org") || !strings.Contains(out, "[2] Transformer (deep learning) — en.wikipedia.org") {
		t.Errorf("unexpected ordering:\n%s", out)
	}
	if strings.Contains(out, "SEO blog") {
		t.Errorf("filtered result should be hidden without --verbose:\n%s", out)
	}
	if !strings.Contains(out, "Score: 9.7/10") {
		t.Errorf("score line missing:\n%s", out)
	}
	if strings.Contains(out, "Probabilities") || strings.Contains(out, "✓ Kept") {
		t.Errorf("verbose-only lines should not appear:\n%s", out)
	}
	if !strings.Contains(errOut, "exa: 3 results → 2 kept (min score 6/10)") {
		t.Errorf("summary missing from stderr: %q", errOut)
	}
}

func TestSearchVerbose(t *testing.T) {
	h := newHarness(t, allKeys())
	out, errOut, err := h.run("--verbose", "--batch", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !h.qual.gotOpts.Batch {
		t.Error("--batch should set QualifyOptions.Batch")
	}
	for _, want := range []string{
		"[3] SEO blog — content-farm.example",
		"Probabilities: {0: 0.05, 1: 0.05, 2: 0.05, 3: 0.80}",
		"✓ Kept",
		"✗ Filtered (below 6 threshold)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "jev: batch mode, 300 input / 3 output tokens") {
		t.Errorf("verbose usage line missing: %q", errOut)
	}
}

func TestSearchJSON(t *testing.T) {
	h := newHarness(t, allKeys())
	out, errOut, err := h.run("--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if errOut != "" {
		t.Errorf("json mode should keep stderr quiet, got %q", errOut)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 2 || items[0].URL != paper.URL || items[1].URL != wiki.URL {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Score == nil || *items[0].Score != 9.7 || items[0].Confidence != nil || !items[0].Kept {
		t.Errorf("item[0] = %+v", items[0])
	}
	if items[0].Yes != nil || items[0].Probability != nil {
		t.Errorf("score mode should omit noul fields: %+v", items[0])
	}
	// jq '.[].url' style access must work: a top-level array of objects.
	generic := mustJSON[[]map[string]any](t, out)
	if generic[0]["url"] != paper.URL {
		t.Errorf("generic[0].url = %v", generic[0]["url"])
	}

	// Verbose JSON includes filtered results, flagged kept=false.
	out, _, err = h.run("--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items = mustJSON[[]outputResult](t, out)
	if len(items) != 3 || items[2].Kept || items[2].URL != blog.URL {
		t.Errorf("verbose items = %+v", items)
	}
}

func TestSearchURLsOnly(t *testing.T) {
	h := newHarness(t, allKeys())
	out, errOut, err := h.run("--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if out != paper.URL+"\n"+wiki.URL+"\n" {
		t.Errorf("out = %q", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestSearchMinScore(t *testing.T) {
	h := newHarness(t, allKeys())
	out, _, err := h.run("--urls-only", "--min-score", "8.5", "q")
	if err != nil {
		t.Fatal(err)
	}
	if out != paper.URL+"\n" {
		t.Errorf("min-score 8.5 should keep only the paper, got %q", out)
	}

	out, _, err = h.run("--urls-only", "-m", "0", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 3 {
		t.Errorf("min-score 0 should keep everything, got %q", out)
	}

	// config.yaml default is honored when the flag is absent.
	if err := os.WriteFile(filepath.Join(h.dir, "config.yaml"), []byte("min_score: 8.5\nnum: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = h.run("--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if out != paper.URL+"\n" || h.prov.gotNum != 4 {
		t.Errorf("config.yaml defaults not applied: out=%q num=%d", out, h.prov.gotNum)
	}
}

func TestSearchNum(t *testing.T) {
	h := newHarness(t, allKeys())
	if _, _, err := h.run("--num", "25", "--no-filter", "q"); err != nil {
		t.Fatal(err)
	}
	if h.prov.gotNum != 25 {
		t.Errorf("num = %d, want 25", h.prov.gotNum)
	}
}

func TestSearchNoFilter(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e"}) // no Jev key at all
	out, errOut, err := h.run("--no-filter", "q")
	if err != nil {
		t.Fatal(err)
	}
	if h.qual.calls != 0 {
		t.Error("--no-filter must not call Jev")
	}
	if !strings.Contains(out, "[1] SEO blog") || !strings.Contains(out, "[3] Transformer (deep learning)") {
		t.Errorf("raw results should keep provider order:\n%s", out)
	}
	if strings.Contains(out, "Score:") || errOut != "" {
		t.Errorf("no-filter should not print scores or a summary: out=%q err=%q", out, errOut)
	}

	out, _, err = h.run("--no-filter", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	raw := mustJSON[[]provider.SearchResult](t, out)
	if len(raw) != 3 || raw[0] != blog {
		t.Errorf("raw json = %+v", raw)
	}

	out, _, err = h.run("--no-filter", "--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if out != blog.URL+"\n"+paper.URL+"\n"+wiki.URL+"\n" {
		t.Errorf("urls = %q", out)
	}
}

func TestSearchNoul(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.scores = map[string]float64{paper.URL: 0.95, wiki.URL: 0.55, blog.URL: 0.2}
	out, errOut, err := h.run("--noul", "Is this a research paper?", "-v", "q")
	if err != nil {
		t.Fatal(err)
	}
	if h.qual.gotOpts.Noul != "Is this a research paper?" {
		t.Errorf("noul question not passed: %+v", h.qual.gotOpts)
	}
	if !strings.Contains(out, "P(yes): 0.95\n    Confidence: 0.90") {
		t.Errorf("noul line missing:\n%s", out)
	}
	if !strings.Contains(errOut, "3 results → 2 kept (P(yes) ≥ 0.50)") {
		t.Errorf("noul default threshold should be 0.5: %q", errOut)
	}

	out, _, err = h.run("--noul", "Paper?", "--min-score", "0.9", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 1 || items[0].Yes == nil || !*items[0].Yes || *items[0].Probability != 0.95 || items[0].Score != nil {
		t.Errorf("noul json = %+v", items)
	}

	if _, _, err := h.run("--noul", "Paper?", "--min-score", "1.5", "q"); err == nil || !strings.Contains(err.Error(), "impossible with --noul") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchRubric(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.scores = map[string]float64{paper.URL: 1.9, wiki.URL: 1.0, blog.URL: 0.1}
	out, _, err := h.run("--rubric", " bad, ok ,great,, ", "--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.qual.gotOpts.Rubric, "|") != "bad|ok|great" {
		t.Errorf("rubric = %v", h.qual.gotOpts.Rubric)
	}
	if out != paper.URL+"\n"+wiki.URL+"\n" {
		t.Errorf("out = %q", out)
	}
	if _, _, err := h.run("--rubric", "only-one", "q"); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := h.run("--rubric", "a,b", "--min-score", "12", "q"); err == nil || !strings.Contains(err.Error(), "exceeds the top score of 10") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchProviderSelection(t *testing.T) {
	// Only Sonar configured → auto-selected even though the default is exa.
	h := newHarness(t, keys.Store{SonarAPIKey: "s", JevAPIKey: "j"})
	if _, _, err := h.run("--urls-only", "q"); err != nil {
		t.Fatal(err)
	}
	if h.prov.name != "sonar" {
		t.Errorf("auto-selected provider = %q, want sonar", h.prov.name)
	}

	// Explicit flag wins.
	h = newHarness(t, allKeys())
	if _, _, err := h.run("-p", "parallel", "--urls-only", "q"); err != nil {
		t.Fatal(err)
	}
	if h.prov.name != "parallel" {
		t.Errorf("provider = %q, want parallel", h.prov.name)
	}

	// Explicit provider without a key is an actionable error.
	h = newHarness(t, keys.Store{ExaAPIKey: "e", JevAPIKey: "j"})
	_, _, err := h.run("--provider", "sonar", "q")
	if err == nil || !strings.Contains(err.Error(), "SONAR_API_KEY") {
		t.Errorf("err = %v", err)
	}
	if h.prov.gotQuery != "" {
		t.Error("should fail before searching")
	}

	// Env var beats keys.json and enables the provider.
	h = newHarness(t, keys.Store{JevAPIKey: "j"})
	t.Setenv("PARALLEL_API_KEY", "from-env")
	if _, _, err := h.run("--urls-only", "q"); err != nil {
		t.Fatal(err)
	}
	if h.prov.name != "parallel" {
		t.Errorf("provider = %q, want parallel from env", h.prov.name)
	}
}

func TestSearchMissingJevKeyFailsBeforeSearch(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e"})
	_, _, err := h.run("q")
	if err == nil || !strings.Contains(err.Error(), "no Jev API key") || !strings.Contains(err.Error(), "--no-filter") {
		t.Errorf("err = %v", err)
	}
	if h.prov.gotQuery != "" {
		t.Error("provider should not be called when the Jev key is missing")
	}
}

func TestSearchFlagConflicts(t *testing.T) {
	h := newHarness(t, allKeys())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--json", "--urls-only", "q"}, "mutually exclusive"},
		{[]string{"--rubric", "a,b", "--noul", "x?", "q"}, "mutually exclusive"},
		{[]string{"   "}, "query is empty"},
	} {
		_, _, err := h.run(tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestSearchNoArgsShowsHelp(t *testing.T) {
	h := newHarness(t, allKeys())
	out, _, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage:") || h.prov.gotQuery != "" {
		t.Errorf("expected help, got:\n%s", out)
	}
}

func TestSearchProviderError(t *testing.T) {
	h := newHarness(t, allKeys())
	h.prov.err = &provider.APIError{Provider: "Exa", Status: 401, Body: "nope"}
	_, _, err := h.run("q")
	if !provider.IsUnauthorized(err) {
		t.Errorf("provider error should propagate, got %v", err)
	}
	if h.qual.calls != 0 {
		t.Error("Jev should not be called after a provider failure")
	}
}

func TestSearchJevError(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.err = errors.New("jev exploded")
	_, _, err := h.run("q")
	if err == nil || !strings.Contains(err.Error(), "jev exploded") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchPerResultJevError(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.errs = map[string]error{wiki.URL: errors.New("timeout")}
	out, errOut, err := h.run("-v", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "1 kept") || !strings.Contains(errOut, "1 not scored (Jev error)") {
		t.Errorf("summary = %q", errOut)
	}
	if !strings.Contains(out, "! Jev error: timeout") || !strings.Contains(out, "✗ Filtered (not scored)") {
		t.Errorf("verbose output should explain the failure:\n%s", out)
	}
	// Errored results sort last, after filtered-but-scored ones.
	if strings.Index(out, "SEO blog") > strings.Index(out, "Transformer (deep learning)") {
		t.Errorf("errored result should sort after scored results:\n%s", out)
	}

	out, _, err = h.run("--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if items := mustJSON[[]outputResult](t, out); len(items) != 1 {
		t.Errorf("errored result must not be kept: %+v", items)
	}
}

func TestSearchNoResults(t *testing.T) {
	h := newHarness(t, allKeys())
	h.prov.results = nil
	out, errOut, err := h.run("q")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" || !strings.Contains(errOut, "returned no results") {
		t.Errorf("out=%q err=%q", out, errOut)
	}
	if h.qual.calls != 0 {
		t.Error("Jev should not be called with zero results")
	}
	out, _, err = h.run("--json", "q")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json with no results = %q, %v", out, err)
	}
	out, _, err = h.run("--json", "--no-filter", "q")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("raw json with no results = %q, %v", out, err)
	}
}

func TestSearchUntitledAndUnparseableURL(t *testing.T) {
	h := newHarness(t, allKeys())
	odd := provider.SearchResult{Title: "  ", URL: "not a url", Snippet: ""}
	h.prov.results = []provider.SearchResult{odd}
	h.qual.scores = map[string]float64{odd.URL: 3}
	out, _, err := h.run("q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[1] (untitled) — ?") {
		t.Errorf("fallback rendering missing:\n%s", out)
	}
}

func TestRank(t *testing.T) {
	// Scores on the four-level rubric; rank compares them scaled to 0–10.
	score := func(s float64) *jev.ScoreAnswer {
		return &jev.ScoreAnswer{Score: s, Legend: map[string]string{"0": "", "1": "", "2": "", "3": ""}}
	}
	in := []jev.Qualified{
		{Result: provider.SearchResult{URL: "low"}, Score: score(0.5)},
		{Result: provider.SearchResult{URL: "err"}, Err: errors.New("x")},
		{Result: provider.SearchResult{URL: "high"}, Score: score(2.5)},
		{Result: provider.SearchResult{URL: "mid"}, Score: score(1.5)},
		{Result: provider.SearchResult{URL: "none"}}, // no answer, no error
	}
	got := rank(in, 3.3)
	var order []string
	for _, r := range got {
		order = append(order, r.Result.URL)
	}
	if strings.Join(order, ",") != "high,mid,low,err,none" {
		t.Errorf("order = %v", order)
	}
	if !got[0].Kept || !got[1].Kept || got[2].Kept || got[3].Kept || got[4].Kept {
		t.Errorf("kept flags wrong: %+v", got)
	}
	kept, failed := countKept(got)
	if kept != 2 || failed != 1 {
		t.Errorf("kept=%d failed=%d", kept, failed)
	}
}

func TestClipSnippet(t *testing.T) {
	if got := clipSnippet("a  b\n\nc", 10); got != "a b c" {
		t.Errorf("clipSnippet = %q", got)
	}
	if got := clipSnippet("héllo wörld", 5); got != "héllo…" {
		t.Errorf("clipSnippet = %q", got)
	}
}

func TestSearchChainFallsBackOnFailure(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e", ParallelAPIKey: "p", SonarAPIKey: "s", JevAPIKey: "j"})
	h.provs = map[string]*fakeProvider{
		"exa":      {err: errors.New("exa is down")},
		"parallel": {err: errors.New("parallel is down")},
		"sonar":    {results: []provider.SearchResult{paper}},
	}
	out, errOut, err := h.run("q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.built, ",") != "exa,parallel,sonar" {
		t.Errorf("chain order = %v, want exa,parallel,sonar", h.built)
	}
	if !strings.Contains(errOut, "exa failed (exa is down); trying parallel") || !strings.Contains(errOut, "parallel failed (parallel is down); trying sonar") {
		t.Errorf("fallback notice missing: %q", errOut)
	}
	if !strings.Contains(errOut, "sonar: 1 results → 1 kept") {
		t.Errorf("summary should name the provider that succeeded: %q", errOut)
	}
	if !strings.Contains(out, "Attention Is All You Need") {
		t.Errorf("output = %q", out)
	}
}

func TestSearchChainAllFail(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e", SearXNGURL: "http://sx", JevAPIKey: "j"})
	h.prov.err = errors.New("boom")
	_, _, err := h.run("q")
	if err == nil || !strings.Contains(err.Error(), "all 2 providers failed") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	if strings.Join(h.built, ",") != "searxng,exa" {
		t.Errorf("chain order = %v", h.built)
	}
}

func TestSearchZeroConfigUsesDDG(t *testing.T) {
	// Nothing configured: the keyless endpoints in order, then ddg.
	h := newHarness(t, keys.Store{})
	_, _, err := h.run("--no-filter", "--urls-only", "--sources", "1", "q")
	if err != nil {
		t.Fatal(err)
	}
	if h.prov.name != "parallel" || strings.Join(h.built, ",") != "parallel" {
		t.Errorf("provider = %q, built = %v; want parallel", h.prov.name, h.built)
	}
	h = newHarness(t, keys.Store{})
	h.provs = map[string]*fakeProvider{
		"parallel":  {err: errors.New("throttled")},
		"exa":       {err: errors.New("throttled")},
		"keenable":  {err: errors.New("throttled")},
		"youcom":    {err: errors.New("throttled")},
		"firecrawl": {err: errors.New("throttled")},
	}
	if _, _, err := h.run("--no-filter", "--urls-only", "--sources", "1", "q"); err != nil || strings.Join(h.built, ",") != "parallel,exa,keenable,youcom,firecrawl,ddg" {
		t.Errorf("zero-config fallback: %v, built = %v", err, h.built)
	}

	// Explicit ddg works without keys too; explicit searxng without a URL does not.
	h = newHarness(t, keys.Store{})
	if _, _, err := h.run("-p", "duckduckgo", "--no-filter", "--urls-only", "q"); err != nil || h.prov.name != "ddg" {
		t.Errorf("explicit ddg: %v, %q", err, h.prov.name)
	}
	_, _, err = h.run("-p", "searxng", "--no-filter", "q")
	if err == nil || !strings.Contains(err.Error(), "SEARXNG_URL") {
		t.Errorf("explicit searxng without URL: %v", err)
	}
}

func TestSearchConfiguredProviderWithoutKeyErrors(t *testing.T) {
	h := newHarness(t, keys.Store{JevAPIKey: "j"})
	t.Setenv("WEBCTL_PROVIDER", "sonar")
	_, _, err := h.run("q")
	if err == nil || !strings.Contains(err.Error(), `configured provider "sonar" is unusable`) {
		t.Errorf("err = %v", err)
	}
	if len(h.built) != 0 {
		t.Error("should fail before constructing any provider")
	}
}

func TestSearchMultiFusesAndTagsEngines(t *testing.T) {
	h := newHarness(t, keys.Store{JevAPIKey: "j", BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{
		"brave": {results: []provider.SearchResult{blog, paper}},
		"exa":   {results: []provider.SearchResult{paper, wiki}},
	}
	out, _, err := h.run("--multi", "--no-filter", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	got := mustJSON[[]rawResult](t, out)
	// paper appears in both lists (ranks 2 and 1) → top; blog (rank 1) beats wiki (rank 2).
	wantURLs := []string{paper.URL, blog.URL, wiki.URL}
	wantEngines := []string{"brave,exa", "brave", "exa"}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	for i := range got {
		if got[i].URL != wantURLs[i] || strings.Join(got[i].Engines, ",") != wantEngines[i] {
			t.Errorf("[%d] = %s %v; want %s %s", i, got[i].URL, got[i].Engines, wantURLs[i], wantEngines[i])
		}
	}

	// Filtered mode: the summary names every engine and engines survive Jev.
	h = newHarness(t, keys.Store{JevAPIKey: "j", BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{
		"brave": {results: []provider.SearchResult{blog, paper}},
		"exa":   {results: []provider.SearchResult{paper, wiki}},
	}
	out, errOut, err := h.run("--multi", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "brave+exa: 3 results → 2 kept") {
		t.Errorf("summary = %q", errOut)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 3 || items[0].URL != paper.URL || strings.Join(items[0].Engines, ",") != "brave,exa" {
		t.Errorf("items = %+v", items)
	}
	out, _, err = h.run("--multi", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Engines: brave, exa") {
		t.Errorf("pretty output should list engines:\n%s", out)
	}
}

func TestSearchMultiPartialFailureAndCap(t *testing.T) {
	h := newHarness(t, keys.Store{JevAPIKey: "j", BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{
		"brave": {err: errors.New("quota")},
		"exa":   {results: []provider.SearchResult{paper, wiki, blog}},
	}
	out, errOut, err := h.run("--multi", "--no-filter", "--urls-only", "-n", "2", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "brave failed: quota") {
		t.Errorf("stderr = %q", errOut)
	}
	if out != paper.URL+"\n"+wiki.URL+"\n" {
		t.Errorf("urls = %q", out)
	}

	h.provs["exa"].err = errors.New("blocked")
	_, _, err = h.run("--multi", "--no-filter", "q")
	if err == nil || !strings.Contains(err.Error(), "all 2 providers failed") || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchRandomFallsBack(t *testing.T) {
	orig := shuffleChain
	shuffleChain = func(chain []string) []string {
		out := append([]string(nil), chain...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	t.Cleanup(func() { shuffleChain = orig })

	h := newHarness(t, keys.Store{JevAPIKey: "j", BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{
		"exa":   {err: errors.New("blocked")},
		"brave": {results: []provider.SearchResult{paper}},
	}
	_, errOut, err := h.run("--random", "--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.built, ",") != "exa,brave" {
		t.Errorf("random order = %v, want reversed chain exa,brave", h.built)
	}
	if !strings.Contains(errOut, "exa failed (blocked); trying brave") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestSearchModeFlagConflicts(t *testing.T) {
	h := newHarness(t, allKeys())
	for _, args := range [][]string{
		{"--multi", "--random", "q"},
		{"--multi", "-p", "exa", "q"},
		{"--random", "-p", "exa", "q"},
	} {
		if _, _, err := h.run(args...); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
	if len(h.built) != 0 {
		t.Error("conflicting flags should fail before any search")
	}
}

// fakeScraper serves canned page text per URL and records what was fetched.
type fakeScraper struct {
	pages    map[string]string // URL → content
	errs     map[string]error  // URL → error
	pdfs     map[string]bool   // URL → served as a PDF
	gotURLs  []string
	maxChars int
}

func (f *fakeScraper) FetchAll(_ context.Context, urls []string, _ int) []scrape.Page {
	f.gotURLs = append(f.gotURLs, urls...)
	out := make([]scrape.Page, len(urls))
	for i, u := range urls {
		out[i] = scrape.Page{URL: u, Content: f.pages[u], Err: f.errs[u], PDF: f.pdfs[u]}
	}
	return out
}

func (h *harness) withScraper(pages map[string]string, errs map[string]error) *fakeScraper {
	fs := &fakeScraper{pages: pages, errs: errs}
	orig := newScraper
	newScraper = func(maxChars int) scraper {
		fs.maxChars = maxChars
		return fs
	}
	h.t.Cleanup(func() { newScraper = orig })
	return fs
}

func TestSearchScrape(t *testing.T) {
	h := newHarness(t, allKeys())
	fs := h.withScraper(
		map[string]string{paper.URL: "Abstract\n\nWe propose the Transformer."},
		map[string]error{wiki.URL: errors.New("HTTP 403")},
	)
	out, errOut, err := h.run("--scrape", "--json", "--verbose", "--max-chars", "1234", "q")
	if err != nil {
		t.Fatal(err)
	}
	// Only kept results are fetched (the blog is dropped by Jev).
	if strings.Join(fs.gotURLs, ",") != paper.URL+","+wiki.URL {
		t.Errorf("fetched %v", fs.gotURLs)
	}
	if fs.maxChars != 1234 {
		t.Errorf("max chars = %d", fs.maxChars)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 3 || items[0].Content != "Abstract\n\nWe propose the Transformer." || items[0].ScrapeError != "" {
		t.Errorf("paper = %+v", items[0])
	}
	if items[1].Content != "" || items[1].ScrapeError != "HTTP 403" {
		t.Errorf("wiki = %+v", items[1])
	}
	if items[2].Content != "" || items[2].ScrapeError != "" {
		t.Errorf("dropped blog should have no content: %+v", items[2])
	}
	if !strings.Contains(errOut, "scraped 2 page(s) (1 failed), 37 chars") {
		t.Errorf("scrape summary missing: %q", errOut)
	}

	out, _, err = h.run("--scrape", "q")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"    --- content (37 chars) ---\n    Abstract\n\n    We propose the Transformer.\n    --- end ---\n",
		"    ! scrape failed: HTTP 403\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty output missing %q:\n%s", want, out)
		}
	}
}

func TestSearchScrapeNoFilterAndURLsOnly(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e"})
	fs := h.withScraper(map[string]string{blog.URL: "b", paper.URL: "p", wiki.URL: "w"}, nil)
	out, _, err := h.run("--scrape", "--no-filter", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	raw := mustJSON[[]rawResult](t, out)
	if len(raw) != 3 || raw[0].Content != "b" || raw[2].Content != "w" || len(fs.gotURLs) != 3 {
		t.Errorf("raw = %+v, fetched %v", raw, fs.gotURLs)
	}
	if !strings.Contains(out, `"content": "b"`) {
		t.Errorf("content field missing:\n%s", out)
	}

	fs.gotURLs = nil
	if _, _, err := h.run("--scrape", "--no-filter", "--urls-only", "q"); err != nil {
		t.Fatal(err)
	}
	if len(fs.gotURLs) != 0 {
		t.Error("--urls-only should not fetch pages")
	}

	if _, _, err := h.run("--scrape", "--max-chars", "0", "q"); err == nil || !strings.Contains(err.Error(), "--max-chars") {
		t.Errorf("err = %v", err)
	}
}

// Three ~1500-char paragraphs: each becomes its own 2000-char chunk.
var (
	chunkA  = strings.TrimSpace(strings.Repeat("Attention lets the model weigh tokens. ", 38))
	chunkB  = strings.TrimSpace(strings.Repeat("Subscribe to our newsletter and accept cookies. ", 31))
	chunkC  = strings.TrimSpace(strings.Repeat("Multi-head attention runs several heads. ", 36))
	bigPage = chunkA + "\n\n" + chunkB + "\n\n" + chunkC
)

func TestSearchFilterChunks(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.withScraper(map[string]string{paper.URL: bigPage, wiki.URL: "Short page about attention."}, nil)

	out, errOut, err := h.run("--scrape", "--filter-chunks", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	if h.qual.chunkCalls != 2 {
		t.Errorf("expected one batch request per page, got %d", h.qual.chunkCalls)
	}
	for _, chunks := range h.qual.gotChunks {
		for _, c := range chunks {
			if len([]rune(c.Text)) > scrape.DefaultChunkChars {
				t.Errorf("chunk of %d runes exceeds %d", len([]rune(c.Text)), scrape.DefaultChunkChars)
			}
		}
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 3 || items[0].URL != paper.URL {
		t.Fatalf("items = %+v", items)
	}
	p := items[0]
	if p.ChunksTotal == nil || *p.ChunksTotal != 3 || p.ChunksKept == nil || *p.ChunksKept != 2 {
		t.Errorf("paper chunk stats = %v/%v", p.ChunksKept, p.ChunksTotal)
	}
	if p.Content != chunkA+"\n\n"+chunkC {
		t.Errorf("paper content = %q", p.Content)
	}
	w := items[1]
	if w.ChunksTotal == nil || *w.ChunksTotal != 1 || *w.ChunksKept != 1 || w.Content != "Short page about attention." {
		t.Errorf("wiki = %+v", w)
	}
	if items[2].ChunksTotal != nil || items[2].Content != "" {
		t.Errorf("dropped result should not be scraped: %+v", items[2])
	}
	if !strings.Contains(errOut, "scraped 2 page(s); chunks 4 → 3 kept; ") || !strings.Contains(errOut, " chars\n") {
		t.Errorf("summary = %q", errOut)
	}

	out, _, err = h.run("--scrape", "--filter-chunks", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "    --- content (2/3 chunks kept, ") || !strings.Contains(out, "    --- end ---") {
		t.Errorf("pretty separator missing:\n%s", out)
	}
	if strings.Contains(out, "newsletter") {
		t.Errorf("dropped chunk leaked into pretty output")
	}
}

func TestSearchFilterChunksWithNoFilter(t *testing.T) {
	// --no-filter skips result qualification but chunk filtering still needs Jev.
	h := newHarness(t, keys.Store{ExaAPIKey: "e"})
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)
	_, _, err := h.run("--scrape", "--filter-chunks", "--no-filter", "q")
	if err == nil || !strings.Contains(err.Error(), "no Jev API key") {
		t.Errorf("err = %v", err)
	}
	if h.prov.gotQuery != "" {
		t.Error("should fail before searching")
	}

	h = newHarness(t, keys.Store{ExaAPIKey: "e", JevAPIKey: "j"})
	h.qual.relevantWord = "attention"
	h.qual.unansweredWord = "Multi-head"
	h.prov.results = []provider.SearchResult{paper}
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)
	out, _, err := h.run("--scrape", "--filter-chunks", "--no-filter", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if h.qual.calls != 0 || h.qual.chunkCalls != 1 {
		t.Errorf("qualify calls = %d, chunk calls = %d", h.qual.calls, h.qual.chunkCalls)
	}
	raw := mustJSON[[]rawResult](t, out)
	// Unanswered chunk (C) is dropped along with the irrelevant one (B).
	if len(raw) != 1 || raw[0].Content != chunkA || *raw[0].ChunksTotal != 3 || *raw[0].ChunksKept != 1 {
		t.Errorf("raw = %+v", raw)
	}
}

func TestSearchFilterChunksJevErrorKeepsContent(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.chunkErr = errors.New("jev down")
	h.prov.results = []provider.SearchResult{paper}
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)
	out, errOut, err := h.run("--scrape", "--filter-chunks", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 1 || items[0].Content != bigPage || items[0].FilterError != "jev down" || *items[0].ChunksKept != 3 {
		t.Errorf("items = %+v", items)
	}
	if !strings.Contains(errOut, "1 page(s) unfiltered (Jev error)") {
		t.Errorf("summary = %q", errOut)
	}
	out, _, err = h.run("--scrape", "--filter-chunks", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "! chunk filter failed: jev down (showing unfiltered content)") {
		t.Errorf("pretty output:\n%s", out)
	}
}

func TestSearchFilterChunksRequiresScrape(t *testing.T) {
	h := newHarness(t, allKeys())
	_, _, err := h.run("--filter-chunks", "q")
	if err == nil || !strings.Contains(err.Error(), "--filter-chunks requires --scrape") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchChainTimesOutSlowProvider(t *testing.T) {
	origAttempt, origBudget := attemptTimeout, chainBudget
	attemptTimeout, chainBudget = 30*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { attemptTimeout, chainBudget = origAttempt, origBudget })

	h := newHarness(t, keys.Store{BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{
		"brave": {hang: true},
		"exa":   {results: []provider.SearchResult{paper}},
	}
	start := time.Now()
	_, errOut, err := h.run("--no-filter", "--urls-only", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.built, ",") != "brave,exa" || !strings.Contains(errOut, "brave failed") {
		t.Errorf("built = %v, stderr = %q", h.built, errOut)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Errorf("slow provider held the chain for %s", time.Since(start))
	}

	// Every provider hanging exhausts the chain budget rather than the sum of attempts.
	attemptTimeout, chainBudget = time.Second, 50*time.Millisecond
	h = newHarness(t, keys.Store{BraveAPIKey: "b", ExaAPIKey: "e"})
	h.provs = map[string]*fakeProvider{"brave": {hang: true}, "exa": {hang: true}}
	start = time.Now()
	_, _, err = h.run("--no-filter", "--urls-only", "q")
	if err == nil || !strings.Contains(err.Error(), "chain budget") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("chain ran %s despite a 50ms budget", time.Since(start))
	}
}

func TestSearchChainTopUpLabel(t *testing.T) {
	h := newHarness(t, keys.Store{JevAPIKey: "j", BraveAPIKey: "b", ExaAPIKey: "e"})
	chainTopUp = true
	h.provs = map[string]*fakeProvider{
		"brave": {results: []provider.SearchResult{paper}},
		"exa":   {results: []provider.SearchResult{wiki, paper}},
	}
	out, errOut, err := h.run("--no-filter", "--urls-only", "-n", "10", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.built, ",") != "brave,exa" || strings.Contains(errOut, "failed") {
		t.Errorf("built = %v, stderr = %q", h.built, errOut)
	}
	if !strings.Contains(out, paper.URL) || !strings.Contains(out, wiki.URL) {
		t.Errorf("fused output = %q", out)
	}
}

func TestSearchFoldsDuplicates(t *testing.T) {
	body := strings.Repeat("Transformers rely entirely on attention to draw global dependencies between input and output tokens. ", 3)
	abs := provider.SearchResult{Title: "Attention Is All You Need", URL: "https://arxiv.org/abs/1706.03762", Snippet: "We propose the Transformer.", Content: body}
	pdf := provider.SearchResult{Title: "Attention Is All You Need PDF", URL: "https://proceedings.neurips.cc/paper/7181.pdf", Snippet: "Abstract.", Content: body + " Footer."}
	h := newHarness(t, keys.Store{JevAPIKey: "j"})
	h.prov.results = []provider.SearchResult{pdf, abs, wiki}
	h.qual.scores = map[string]float64{abs.URL: 2.9, pdf.URL: 2.5, wiki.URL: 2.2}
	h.qual.dupes = map[string]bool{abs.URL + "|" + pdf.URL: true}
	out, errOut, err := h.run("--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 2 || items[0].URL != abs.URL || strings.Join(items[0].Duplicates, ",") != pdf.URL {
		t.Errorf("items = %+v", items)
	}
	if !strings.Contains(errOut, "1 duplicate(s) folded") {
		t.Errorf("stderr = %q", errOut)
	}
	// --no-dedupe keeps both.
	out, _, err = h.run("--json", "--no-dedupe", "q")
	if err != nil {
		t.Fatal(err)
	}
	if items := mustJSON[[]outputResult](t, out); len(items) != 3 {
		t.Errorf("no-dedupe items = %d", len(items))
	}
}

func TestSearchScrapeAlwaysChunksPDFs(t *testing.T) {
	h := newHarness(t, keys.Store{JevAPIKey: "j"})
	long := strings.Repeat("A paragraph of the paper's text that is long enough to become its own chunk when split. ", 40)
	fs := &fakeScraper{
		pages: map[string]string{paper.URL: long, wiki.URL: long},
		pdfs:  map[string]bool{paper.URL: true},
	}
	orig := newScraper
	newScraper = func(int) scraper { return fs }
	t.Cleanup(func() { newScraper = orig })
	h.qual.relevantWord = "paragraph"

	// No --filter-chunks: the PDF is chunk-filtered anyway, the HTML page is not.
	out, errOut, err := h.run("--scrape", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	byURL := map[string]outputResult{}
	for _, it := range items {
		byURL[it.URL] = it
	}
	pdf, html := byURL[paper.URL], byURL[wiki.URL]
	if pdf.PDF == nil || !*pdf.PDF || pdf.ChunksTotal == nil || *pdf.ChunksTotal == 0 {
		t.Errorf("pdf result should be flagged and chunk-filtered: %+v", pdf)
	}
	if html.PDF != nil || html.ChunksTotal != nil {
		t.Errorf("html result should be untouched without --filter-chunks: %+v", html)
	}
	if !strings.Contains(errOut, "chunks") {
		t.Errorf("summary should mention chunks: %q", errOut)
	}
	// --no-filter has no Jev: the PDF text is delivered whole.
	out, _, err = h.run("--scrape", "--no-filter", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range mustJSON[[]rawResult](t, out) {
		if it.URL == paper.URL && (it.PDF == nil || it.ChunksTotal != nil) {
			t.Errorf("no-filter pdf = %+v", it)
		}
	}
}

func TestSearchMinResultsBackfills(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.scores = map[string]float64{paper.URL: 2.9, wiki.URL: 1.4, blog.URL: 0.3}
	out, errOut, err := h.run("--json", "--verbose", "--min-results", "2", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	kept := map[string]outputResult{}
	for _, it := range items {
		if it.Kept {
			kept[it.URL] = it
		}
	}
	if len(kept) != 2 || kept[paper.URL].Backfilled || !kept[wiki.URL].Backfilled {
		t.Errorf("kept = %+v", kept)
	}
	if _, ok := kept[blog.URL]; ok {
		t.Error("a result under the 1.0 floor must never be backfilled")
	}
	if !strings.Contains(errOut, "1 backfilled toward --min-results 2") || strings.Contains(errOut, "not reached") {
		t.Errorf("summary = %q", errOut)
	}
	// Asking for more than the floor allows reports the shortfall.
	_, errOut, err = h.run("--urls-only", "--verbose", "--min-results", "3", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "was not reached") {
		t.Errorf("shortfall summary = %q", errOut)
	}
	// Without the flag nothing is promoted.
	out, _, err = h.run("--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range mustJSON[[]outputResult](t, out) {
		if it.Backfilled || (it.URL == wiki.URL && it.Kept) {
			t.Errorf("unexpected keep without --min-results: %+v", it)
		}
	}
}

func TestSearchGoalReachesJudges(t *testing.T) {
	h := newHarness(t, allKeys())
	_, _, err := h.run("search", "giants score september 19", "--goal", "the final score of last night's Giants game", "--urls-only")
	if err != nil {
		t.Fatal(err)
	}
	if h.qual.gotQuery != "giants score september 19" || h.qual.gotGoal != "the final score of last night's Giants game" {
		t.Errorf("judges saw query %q goal %q", h.qual.gotQuery, h.qual.gotGoal)
	}
	// The bare form still works and carries no goal.
	_, _, err = h.run("giants score", "--urls-only")
	if err != nil || h.qual.gotGoal != "" || h.qual.gotQuery != "giants score" {
		t.Errorf("bare form: %v query %q goal %q", err, h.qual.gotQuery, h.qual.gotGoal)
	}
}

// A page whose batches partly failed keeps the chunks Jev never ruled on and
// still drops the ones it rejected, rather than falling back to the whole page.
func TestSearchFilterChunksPartialFailureKeepsUnjudged(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.qual.chunkUnjudged = []int{0}
	h.prov.results = []provider.SearchResult{paper}
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)

	out, errOut, err := h.run("--scrape", "--filter-chunks", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	got := items[0]
	// chunkA unjudged (kept), chunkB rejected (dropped), chunkC relevant (kept).
	if got.Content != chunkA+"\n\n"+chunkC {
		t.Errorf("content should keep unjudged + relevant chunks only, got %d chars", len(got.Content))
	}
	if got.ChunksKept == nil || *got.ChunksKept != 2 {
		t.Errorf("chunks_kept = %v, want 2", got.ChunksKept)
	}
	if got.ChunksUnjudged == nil || *got.ChunksUnjudged != 1 {
		t.Errorf("chunks_unjudged = %v, want 1", got.ChunksUnjudged)
	}
	if !strings.Contains(got.FilterError, "unjudged") {
		t.Errorf("filter_error = %q", got.FilterError)
	}
	if !strings.Contains(errOut, "partly filtered") {
		t.Errorf("summary = %q", errOut)
	}

	out, _, err = h.run("--scrape", "--filter-chunks", "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "chunk filter partly failed") || !strings.Contains(out, "1 unjudged") {
		t.Errorf("pretty output:\n%s", out)
	}
}

// The judge sees each chunk with the tail of the previous one as context;
// the kept text is the bare chunk. --chunk-chars changes the chunk size.
func TestSearchFilterChunksOverlapAndChunkChars(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.prov.results = []provider.SearchResult{paper}
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)

	out, _, err := h.run("--scrape", "--filter-chunks", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.qual.gotChunks) != 1 || len(h.qual.gotChunks[0]) != 3 {
		t.Fatalf("judged chunks = %v", h.qual.gotChunks)
	}
	judged := h.qual.gotChunks[0]
	if judged[0].Before != "" || judged[0].Text != chunkA {
		t.Errorf("first chunk should have no context")
	}
	if judged[1].Text != chunkB || !strings.HasPrefix(judged[1].Before, "Attention lets") || !strings.HasSuffix(chunkA, judged[1].Before) {
		t.Errorf("second chunk should carry a sentence-aligned tail of chunkA as context: %q", judged[1].Before)
	}
	want := scrape.Overlap(scrape.DefaultChunkChars)
	if n := len([]rune(judged[1].Before)); n > want || n < want/2 {
		t.Errorf("context = %d runes, want about %d", n, want)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 1 || items[0].Content != chunkA+"\n\n"+chunkC {
		t.Errorf("kept content must be the bare chunks: %q", items[0].Content)
	}

	// Smaller chunks: more of them, context scaled down, still bare on output.
	h = newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.prov.results = []provider.SearchResult{paper}
	h.withScraper(map[string]string{paper.URL: bigPage}, nil)
	out, _, err = h.run("--scrape", "--filter-chunks", "--chunk-chars", "500", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	judged = h.qual.gotChunks[0]
	if len(judged) < 8 {
		t.Errorf("500-char chunks of a %d-char page: got %d chunks", len(bigPage), len(judged))
	}
	for i, c := range judged {
		if n := len([]rune(c.Before)); n > scrape.Overlap(500) || (i > 0 && n == 0) {
			t.Errorf("chunk %d context = %d runes, want 1..%d", i, n, scrape.Overlap(500))
		}
	}
	items = mustJSON[[]outputResult](t, out)
	if items[0].ChunksTotal == nil || *items[0].ChunksTotal < 8 || strings.Contains(items[0].Content, "cookies") {
		t.Errorf("chunks_total = %v, newsletter text leaked: %v", items[0].ChunksTotal, strings.Contains(items[0].Content, "cookies"))
	}

	if _, _, err := h.run("--scrape", "--filter-chunks", "--chunk-chars", "0", "q"); err == nil || !strings.Contains(err.Error(), "--chunk-chars") {
		t.Errorf("zero chunk size should be rejected: %v", err)
	}
}

func TestSearchScrapeTop(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.scores = map[string]float64{paper.URL: 2.9, wiki.URL: 2.4, blog.URL: 2.0}
	fs := h.withScraper(map[string]string{paper.URL: "p", wiki.URL: "w", blog.URL: "b"}, nil)

	// Default: the three kept results all fit under --scrape-top 3.
	out, _, err := h.run("--scrape", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fs.gotURLs, ",") != paper.URL+","+wiki.URL+","+blog.URL {
		t.Errorf("default fetched %v", fs.gotURLs)
	}

	// --scrape-top 2 fetches the two best; the third keeps its snippet only.
	fs.gotURLs = nil
	out, _, err = h.run("--scrape", "--scrape-top", "2", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fs.gotURLs, ",") != paper.URL+","+wiki.URL {
		t.Errorf("top 2 fetched %v", fs.gotURLs)
	}
	items := mustJSON[[]outputResult](t, out)
	if len(items) != 3 || items[0].Content != "p" || items[1].Content != "w" || items[2].Content != "" || items[2].Snippet == "" {
		t.Errorf("items = %+v", items)
	}

	// 0 means every kept result.
	fs.gotURLs = nil
	if _, _, err := h.run("--scrape", "--scrape-top", "0", "--urls-only", "q"); err != nil {
		t.Fatal(err)
	}
	fs.gotURLs = nil
	if _, _, err := h.run("--scrape", "--scrape-top", "0", "--json", "q"); err != nil {
		t.Fatal(err)
	}
	if len(fs.gotURLs) != 3 {
		t.Errorf("top 0 fetched %v", fs.gotURLs)
	}

	// --no-filter takes the fused order.
	fs.gotURLs = nil
	out, _, err = h.run("--scrape", "--no-filter", "--scrape-top", "1", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	raw := mustJSON[[]rawResult](t, out)
	if strings.Join(fs.gotURLs, ",") != blog.URL || len(raw) != 3 || raw[0].Content != "b" || raw[1].Content != "" {
		t.Errorf("no-filter fetched %v, raw = %+v", fs.gotURLs, raw)
	}

	if _, _, err := h.run("--scrape", "--scrape-top", "-1", "q"); err == nil || !strings.Contains(err.Error(), "--scrape-top") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchScrapeSkipsBackfilled(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.scores = map[string]float64{paper.URL: 2.9, wiki.URL: 1.4, blog.URL: 0.3}
	fs := h.withScraper(map[string]string{paper.URL: "p", wiki.URL: "w"}, nil)
	out, _, err := h.run("--scrape", "--min-results", "2", "--json", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fs.gotURLs, ",") != paper.URL {
		t.Errorf("fetched %v", fs.gotURLs)
	}
	for _, it := range mustJSON[[]outputResult](t, out) {
		if it.Backfilled && it.Content != "" {
			t.Errorf("backfilled result was scraped: %+v", it)
		}
	}
}

func TestSearchMaxOutput(t *testing.T) {
	h := newHarness(t, allKeys())
	h.withScraper(map[string]string{paper.URL: bigPage, wiki.URL: bigPage}, nil)

	out, errOut, err := h.run("--scrape", "--max-output", "3000", "q")
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(out); n > 3000 {
		t.Errorf("output is %d runes, over the 3000 budget", n)
	}
	// Both headers print; the first page keeps most of the budget, the
	// second is cut, and both are marked.
	for _, want := range []string{paper.URL, wiki.URL, chunkA, "more chars trimmed by --max-output)\n    --- end ---"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "trimmed by --max-output") != 2 {
		t.Errorf("expected both pages marked:\n%s", out)
	}
	if !strings.Contains(errOut, "--max-output 3000: trimmed ") || !strings.Contains(errOut, " chars of scraped content from 2 page(s)") {
		t.Errorf("summary = %q", errOut)
	}

	// JSON: content fields are bounded and report what was cut.
	out, errOut, err = h.run("--scrape", "--max-output", "3000", "--json", "--verbose", "q")
	if err != nil {
		t.Fatal(err)
	}
	items := mustJSON[[]outputResult](t, out)
	total := 0
	for _, it := range items {
		total += utf8.RuneCountInString(it.Content)
	}
	if total > 3000 || items[0].CharsTrimmed == nil || items[1].CharsTrimmed == nil {
		t.Errorf("json content total %d, items = %+v", total, items)
	}
	if utf8.RuneCountInString(bigPage)-*items[0].CharsTrimmed != utf8.RuneCountInString(items[0].Content) {
		t.Errorf("chars_trimmed %d does not account for %d → %d", *items[0].CharsTrimmed, utf8.RuneCountInString(bigPage), utf8.RuneCountInString(items[0].Content))
	}
	if !strings.Contains(errOut, "--max-output 3000") {
		t.Errorf("json verbose summary = %q", errOut)
	}

	// 0 is unlimited; the default leaves this small run untouched.
	for _, args := range [][]string{{"--scrape", "--max-output", "0", "q"}, {"--scrape", "q"}} {
		out, errOut, err := h.run(args...)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "trimmed") || strings.Contains(errOut, "trimmed") || strings.Count(out, chunkC) != 2 {
			t.Errorf("%v trimmed:\n%s%s", args, out, errOut)
		}
	}
	if _, _, err := h.run("--max-output", "-5", "q"); err == nil || !strings.Contains(err.Error(), "--max-output") {
		t.Errorf("err = %v", err)
	}
}

func TestBudgetOutput(t *testing.T) {
	pages := []*pageContent{nil, {Content: "one two.\n\nthree four.\n\nfive six."}, {Content: "seven eight."}}
	render := func(i int) string {
		if pages[i] == nil {
			return "header\n"
		}
		return "header\n" + pages[i].Content + "\n"
	}
	rendered, n, chars := budgetOutput(31, pages, render)
	if n != 2 || chars != 36 || pages[1].Content != "one two." || pages[2].Content != "" {
		t.Errorf("n=%d chars=%d pages=%+v", n, chars, []pageContent{*pages[1], *pages[2]})
	}
	total := 0
	for _, s := range rendered {
		total += len(s)
	}
	if total > 31 || rendered[0] != "header\n" {
		t.Errorf("rendered %d runes: %q", total, rendered)
	}
	// Unlimited leaves everything alone.
	pages[1].Content, pages[1].Trimmed = "x\n\ny", 0
	if _, n, _ := budgetOutput(0, pages[:2], render); n != 0 || pages[1].Content != "x\n\ny" {
		t.Errorf("unlimited trimmed: n=%d %q", n, pages[1].Content)
	}
}

func TestCutAtBoundaryAndCommas(t *testing.T) {
	for _, c := range []struct {
		s    string
		n    int
		want string
	}{
		{"a b.\n\nc d.\n\ne f.", 12, "a b.\n\nc d."},
		{"a b.\n\nc d.\n\ne f.", 8, "a b."},
		{"line one\nline two", 12, "line one"},
		{"word word word", 9, "word"},
		{"nospace", 3, "nos"},
		{"short", 10, "short"},
		{"anything", 0, ""},
	} {
		if got := cutAtBoundary(c.s, c.n); got != c.want {
			t.Errorf("cutAtBoundary(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 12400: "12,400", 1234567: "1,234,567"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q", n, got)
		}
	}
}

type fakeSummarizer struct {
	mu    sync.Mutex
	calls []summarize.Input
	fail  bool
}

func (f *fakeSummarizer) Name() string { return "fake" }
func (f *fakeSummarizer) Summarize(_ context.Context, in summarize.Input) (string, summarize.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.fail {
		return "", summarize.Usage{}, errors.New("model down")
	}
	if strings.Contains(strings.Join(in.Chunks, " "), "Multi-head") {
		return "SUMMARY: attention weighs tokens; multi-head runs several heads.", summarize.Usage{InputTokens: 100, OutputTokens: 12}, nil
	}
	return "nothing relevant", summarize.Usage{InputTokens: 20, OutputTokens: 2}, nil
}

// --summarize replaces kept text with the model's summary, drops pages the
// model calls irrelevant, and falls back to the kept text on failure.
func TestSearchSummarize(t *testing.T) {
	fs := &fakeSummarizer{}
	orig := newSummarizer
	newSummarizer = func(cfg summarize.Config) (summarize.Summarizer, error) {
		if cfg.Command != "fake-cmd" {
			t.Errorf("command = %q", cfg.Command)
		}
		return fs, nil
	}
	t.Cleanup(func() { newSummarizer = orig })

	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.prov.results = []provider.SearchResult{paper, wiki}
	h.withScraper(map[string]string{paper.URL: bigPage, wiki.URL: "Short page about attention."}, nil)
	out, errOut, err := h.run("--scrape", "--filter-chunks", "--summarize", "--summarize-command", "fake-cmd", "--json", "--verbose", "q", "--goal", "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 2 || fs.calls[0].Goal != "g" || fs.calls[0].Query != "q" {
		t.Fatalf("summarizer calls = %+v", fs.calls)
	}
	items := mustJSON[[]outputResult](t, out)
	byURL := map[string]outputResult{}
	for _, it := range items {
		byURL[it.URL] = it
	}
	if p := byURL[paper.URL]; !strings.HasPrefix(p.Content, "SUMMARY:") || p.Summarized == nil || !*p.Summarized {
		t.Errorf("paper = %+v", p)
	}
	if w := byURL[wiki.URL]; w.Content != "" || w.Summarized == nil {
		t.Errorf("nothing-relevant page should have empty summarized content: %+v", w)
	}
	if !strings.Contains(errOut, "summarized 2 page(s)") || !strings.Contains(errOut, "1 with nothing relevant") || !strings.Contains(errOut, "120 in / 14 out tokens") {
		t.Errorf("summary line = %q", errOut)
	}

	// Pretty output shows the summary header; a failure shows the kept text.
	out, _, err = h.run("--scrape", "--filter-chunks", "--summarize", "--summarize-command", "fake-cmd", "q")
	if err != nil || !strings.Contains(out, "--- summary (") || !strings.Contains(out, "nothing relevant in") {
		t.Errorf("pretty:\n%s (%v)", out, err)
	}
	fs.fail = true
	out, errOut, err = h.run("--scrape", "--filter-chunks", "--summarize", "--summarize-command", "fake-cmd", "q")
	if err != nil || !strings.Contains(out, "! summarize failed: model down") || !strings.Contains(out, "Multi-head attention") || !strings.Contains(errOut, "not summarized") {
		t.Errorf("failure fallback:\n%s\n%s (%v)", out, errOut, err)
	}

	// Guard rails.
	if _, _, err := h.run("--summarize", "q"); err == nil || !strings.Contains(err.Error(), "--scrape") {
		t.Errorf("summarize without scrape: %v", err)
	}
	newSummarizer = summarize.New
	if _, _, err := h.run("--scrape", "--summarize", "q"); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Errorf("unconfigured summarize: %v", err)
	}
}
