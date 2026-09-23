package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/dorkitude/webctl/internal/keys"
	"github.com/dorkitude/webctl/internal/summarize"
)

// threeParagraphs is short enough that --chunk-chars 40 puts every paragraph
// in its own chunk, so the filter's choice is visible in the output.
const threeParagraphs = "Attention weighs every token pair.\n\n" +
	"The cafeteria menu changes weekly here.\n\n" +
	"Attention uses several parallel heads."

// fetch returns only the chunks Jev judges relevant, and reaches no search
// provider: it costs no search quota and works with every provider cooling
// down.
func TestFetchKeepsRelevantChunksAndSearchesNothing(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	fs := h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)

	out, _, err := h.run("fetch", paper.URL, "--goal", "how attention works", "--chunk-chars", "40")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.gotURLs) != 1 || fs.gotURLs[0] != paper.URL {
		t.Errorf("fetched %v, want [%s]", fs.gotURLs, paper.URL)
	}
	if len(h.built) != 0 {
		t.Errorf("built search providers %v, want none", h.built)
	}
	if !strings.Contains(out, "Attention weighs every token pair.") {
		t.Errorf("relevant chunk missing from output:\n%s", out)
	}
	if !strings.Contains(out, "Attention uses several parallel heads.") {
		t.Errorf("second relevant chunk missing from output:\n%s", out)
	}
	if strings.Contains(out, "cafeteria") {
		t.Errorf("irrelevant chunk kept:\n%s", out)
	}
	if !strings.Contains(out, paper.URL) {
		t.Errorf("URL missing from output:\n%s", out)
	}
}

// The goal is the only thing the judges have here, so it must reach them.
func TestFetchRequiresGoal(t *testing.T) {
	h := newHarness(t, allKeys())
	h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)

	_, _, err := h.run("fetch", paper.URL)
	if err == nil || !strings.Contains(err.Error(), "--goal is required") {
		t.Fatalf("error = %v, want --goal is required", err)
	}
}

// A bad URL fails before anything is fetched.
func TestFetchRejectsNonHTTPURL(t *testing.T) {
	for _, arg := range []string{"file:///etc/passwd", "example.com/page", "https://"} {
		h := newHarness(t, allKeys())
		fs := h.withScraper(map[string]string{}, nil)
		_, _, err := h.run("fetch", arg, "--goal", "g")
		if err == nil {
			t.Errorf("%q: want an error", arg)
		}
		if len(fs.gotURLs) != 0 {
			t.Errorf("%q: fetched %v before validating", arg, fs.gotURLs)
		}
	}
}

// The Jev key is checked before any page is fetched, since the chunk filter
// runs on every one of them.
func TestFetchMissingJevKeyFailsBeforeFetching(t *testing.T) {
	h := newHarness(t, keys.Store{ExaAPIKey: "e"})
	fs := h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)

	_, _, err := h.run("fetch", paper.URL, "--goal", "g")
	if err == nil {
		t.Fatal("want an error without a Jev key")
	}
	if len(fs.gotURLs) != 0 {
		t.Errorf("fetched %v before checking the key", fs.gotURLs)
	}
}

// Every URL is reported, and one that cannot be fetched does not stop the
// others.
func TestFetchReportsPerPageErrors(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.withScraper(
		map[string]string{paper.URL: threeParagraphs},
		map[string]error{wiki.URL: errors.New("403 forbidden")},
	)

	out, _, err := h.run("fetch", paper.URL, wiki.URL, "--goal", "g", "--chunk-chars", "40")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "403 forbidden") {
		t.Errorf("fetch error missing from output:\n%s", out)
	}
	if !strings.Contains(out, "Attention weighs every token pair.") {
		t.Errorf("the page that worked is missing:\n%s", out)
	}
}

func TestFetchJSON(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)

	out, _, err := h.run("fetch", paper.URL, "--goal", "g", "--chunk-chars", "40", "--json")
	if err != nil {
		t.Fatal(err)
	}
	got := mustJSON[[]fetchResult](t, out)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	r := got[0]
	if r.URL != paper.URL {
		t.Errorf("url = %q", r.URL)
	}
	if strings.Contains(r.Content, "cafeteria") {
		t.Errorf("content kept an irrelevant chunk: %q", r.Content)
	}
	if r.ChunksTotal == nil || *r.ChunksTotal != 3 {
		t.Errorf("chunks_total = %v, want 3", r.ChunksTotal)
	}
	if r.ChunksKept == nil || *r.ChunksKept != 2 {
		t.Errorf("chunks_kept = %v, want 2", r.ChunksKept)
	}
}

// --summarize replaces the kept text, and the summarizer is told the goal.
func TestFetchSummarize(t *testing.T) {
	sum := &fakeSummarizer{}
	orig := newSummarizer
	newSummarizer = func(cfg summarize.Config) (summarize.Summarizer, error) {
		if cfg.Command != "fake-cmd" {
			t.Errorf("command = %q", cfg.Command)
		}
		return sum, nil
	}
	t.Cleanup(func() { newSummarizer = orig })

	h := newHarness(t, allKeys())
	h.qual.relevantWord = "multi-head"
	h.withScraper(map[string]string{paper.URL: "Multi-head attention runs several heads."}, nil)

	out, _, err := h.run("fetch", paper.URL, "--goal", "how attention works",
		"--summarize", "--summarize-command", "fake-cmd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SUMMARY: attention weighs tokens") {
		t.Errorf("summary missing from output:\n%s", out)
	}
	if len(sum.calls) != 1 {
		t.Fatalf("summarizer called %d times, want 1", len(sum.calls))
	}
	if sum.calls[0].Goal != "how attention works" {
		t.Errorf("summarizer goal = %q", sum.calls[0].Goal)
	}
	if sum.calls[0].URL != paper.URL {
		t.Errorf("summarizer url = %q", sum.calls[0].URL)
	}
}

// --summarize-model reaches a command backend when no endpoint is configured,
// and keeps selecting the endpoint when one is.
func TestSummarizeModelRouting(t *testing.T) {
	var got summarize.Config
	orig := newSummarizer
	newSummarizer = func(cfg summarize.Config) (summarize.Summarizer, error) {
		got = cfg
		return &fakeSummarizer{}, nil
	}
	t.Cleanup(func() { newSummarizer = orig })

	h := newHarness(t, allKeys())
	h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)
	t.Setenv("WEBCTL_SUMMARIZE_COMMAND", "cfg-cmd")
	t.Setenv("WEBCTL_SUMMARIZE_ENDPOINT", "")
	t.Setenv("WEBCTL_SUMMARIZE_MODEL", "")

	if _, _, err := h.run("fetch", paper.URL, "--goal", "g", "--summarize", "--summarize-model", "haiku"); err != nil {
		t.Fatal(err)
	}
	if got.Backend() != summarize.BackendCommand || got.Command != "cfg-cmd" || got.Model != "haiku" {
		t.Errorf("command only: %s backend %q with model %q, want the command with haiku", got.Backend(), got.Command, got.Model)
	}

	t.Setenv("WEBCTL_SUMMARIZE_ENDPOINT", "https://api.example/v1")
	if _, _, err := h.run("fetch", paper.URL, "--goal", "g", "--summarize", "--summarize-model", "big-model"); err != nil {
		t.Fatal(err)
	}
	if got.Backend() != summarize.BackendEndpoint || got.Model != "big-model" {
		t.Errorf("with an endpoint: %s backend with model %q, want the endpoint with big-model", got.Backend(), got.Model)
	}
}

// --max-output trims page content and says so, while the header that names
// the page always survives.
func TestFetchMaxOutput(t *testing.T) {
	h := newHarness(t, allKeys())
	h.qual.relevantWord = "attention"
	h.withScraper(map[string]string{paper.URL: threeParagraphs}, nil)

	out, errOut, err := h.run("fetch", paper.URL, "--goal", "g", "--chunk-chars", "40", "--max-output", "120")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Attention weighs every token pair.") {
		t.Errorf("content survived the budget:\n%s", out)
	}
	if !strings.Contains(out, "trimmed by --max-output") {
		t.Errorf("trimming was not reported:\n%s", out)
	}
	if !strings.Contains(out, paper.URL) {
		t.Errorf("the header was trimmed away:\n%s", out)
	}
	if !strings.Contains(errOut, "--max-output 120: trimmed") {
		t.Errorf("no budget summary on stderr:\n%s", errOut)
	}
}

func TestFetchNoArgsShowsHelp(t *testing.T) {
	h := newHarness(t, allKeys())
	out, _, err := h.run("fetch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "webctl fetch") {
		t.Errorf("help missing:\n%s", out)
	}
}
