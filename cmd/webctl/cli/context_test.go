package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/dorkitude/webctl/internal/keys"
	"github.com/dorkitude/webctl/internal/provider"
)

func clearSummarizeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"COMMAND", "ENDPOINT", "MODEL"} {
		t.Setenv("WEBCTL_SUMMARIZE_"+k, "")
	}
}

// With no search key, no summarizer, and the Jev key borrowed from
// TypeSafe's variable, the briefing says exactly that: the keyless chain
// throttles, and --summarize is neither available nor advertised.
func TestContextReportsWhatWillNotWork(t *testing.T) {
	h := newHarness(t, keys.Store{})
	clearSummarizeEnv(t)
	t.Setenv("TYPESAFE_API_KEY", "ts")

	out, _, err := h.run("context")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"jev: configured via TYPESAFE_API_KEY\n",
		"search: parallel, exa, keenable, youcom, firecrawl, ddg (keyless endpoints throttle",
		"cooling_down: none\n",
		`summarize: "unavailable; summarize: set summarize.command or summarize.endpoint`,
		"commands[2]{command,use}:\n",
		"help[3]:\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--summarize-model") {
		t.Errorf("advertises --summarize-model with no backend:\n%s", out)
	}
}

// Keyed providers replace the keyless chain, and a configured summarizer
// adds its help line. Nothing reaches a provider or Jev: the briefing is
// read from local state only.
func TestContextWithKeysAndSummarizer(t *testing.T) {
	h := newHarness(t, allKeys())
	clearSummarizeEnv(t)
	t.Setenv("WEBCTL_SUMMARIZE_COMMAND", "some-cmd")
	t.Setenv("WEBCTL_SUMMARIZE_MODEL", "haiku")

	out, _, err := h.run("context")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"jev: configured\n",
		"search: exa, parallel, sonar\n",
		"summarize: command backend, model haiku\n",
		"help[4]:\n",
		"--summarize-model <name> picks the model for one call",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(h.built) != 0 || h.qual.calls != 0 || h.qual.chunkCalls != 0 {
		t.Errorf("context made calls: providers %v, jev %d/%d", h.built, h.qual.calls, h.qual.chunkCalls)
	}
}

// An endpoint without a model is configured but would fail, so the briefing
// says why and leaves --summarize out of help.
func TestContextSummarizerThatWouldFail(t *testing.T) {
	h := newHarness(t, allKeys())
	clearSummarizeEnv(t)
	t.Setenv("WEBCTL_SUMMARIZE_ENDPOINT", "http://localhost:11434/v1")

	out, _, err := h.run("context")
	if err != nil {
		t.Fatal(err)
	}
	if want := `summarize: "unavailable; summarize: endpoint needs a model`; !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
	if strings.Contains(out, "--summarize") {
		t.Errorf("advertises --summarize with a backend that would fail:\n%s", out)
	}
}

func TestContextMissingJevKey(t *testing.T) {
	h := newHarness(t, keys.Store{})
	clearSummarizeEnv(t)

	out, _, err := h.run("context")
	if err != nil {
		t.Fatal(err)
	}
	if want := "jev: missing; fetch fails and search needs --no-filter until JEV_API_KEY or TYPESAFE_API_KEY is set\n"; !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

// Only providers whose window is still open are listed, with the time left.
func TestCoolingState(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	entries := map[string]provider.CooldownEntry{
		"exa":      {Until: now.Add(42 * time.Minute)},
		"parallel": {Until: now.Add(-time.Minute)},
	}
	want := "exa for " + provider.FormatDuration(42*time.Minute)
	if got := coolingState(entries, now); got != want {
		t.Errorf("coolingState = %q, want %q", got, want)
	}
	if got := coolingState(nil, now); got != "none" {
		t.Errorf("coolingState(nil) = %q, want none", got)
	}
}

func TestToonValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"command backend, model haiku", "command backend, model haiku"},
		{"0.1.6-9d5fa97", "0.1.6-9d5fa97"},
		{"", `""`},
		{"qwen3:8b", `"qwen3:8b"`},
		{"true", `"true"`},
		{"42", `"42"`},
		{"-dash", `"-dash"`},
		{` padded`, `" padded"`},
		{`say "hi"`, `"say \"hi\""`},
	} {
		if got := toonValue(tc.in); got != tc.want {
			t.Errorf("toonValue(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
