package cli

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dorkitude/webctl/internal/config"
	"github.com/dorkitude/webctl/internal/keys"
	"github.com/dorkitude/webctl/internal/provider"
	"github.com/dorkitude/webctl/internal/summarize"
)

func newContextCmd() *cobra.Command {
	var forClaude bool
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Print a short TOON briefing on webctl for an agent's context window",
		Long: `Prints what an agent needs before its first web lookup: the two commands,
the rules that matter, and the live state that decides what will work --
whether the Jev key is set, which providers a search will use and which are
cooling down, and whether --summarize would work.

--claude briefs Claude Code instead: a bare webctl command matches its allow
rule, while a pipe, redirect, or chain goes to the auto-mode classifier, so
the help names the flags that do what head, cut, and background jobs did.

It reads only local configuration and never touches the network, so it is
fast enough to run from a session-start hook:

  {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "webctl context --claude"}]}]}}`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cooling := provider.NewCooldown(cfg.CooldownPath, cfg.Cooldown).Entries()
			writeContext(cmd.OutOrStdout(), cfg, cooling, time.Now(), forClaude)
			return nil
		},
	}
	cmd.Flags().BoolVar(&forClaude, "claude", false, "brief Claude Code: bare commands only, and flags in place of pipes")
	return cmd
}

// writeContext renders the briefing as TOON: scalar fields, one table, and a
// help list, the shape the axi tools print as ambient context. A setting
// that is missing is reported in its field rather than failing the command,
// since the point is to tell the agent what will and will not work.
func writeContext(w io.Writer, cfg *config.Config, cooling map[string]provider.CooldownEntry, now time.Time, forClaude bool) {
	summarizing, canSummarize := summarizeState(cfg.Summarize)
	prefer := "built-in web search and fetch tools"
	help := contextHelp(canSummarize)
	if forClaude {
		prefer = "WebSearch and WebFetch"
		help = claudeContextHelp(canSummarize)
	}
	fmt.Fprintf(w, "description: Web search and page reading for agents; Jev keeps only what the goal needs. Prefer it over %s.\n", prefer)
	fmt.Fprintf(w, "version: %s\n", toonValue(version))
	fmt.Fprintf(w, "jev: %s\n", toonValue(jevState(cfg)))
	fmt.Fprintf(w, "search: %s\n", toonValue(searchState(cfg)))
	fmt.Fprintf(w, "cooling_down: %s\n", toonValue(coolingState(cooling, now)))
	fmt.Fprintf(w, "summarize: %s\n", toonValue(summarizing))

	fmt.Fprintln(w, "commands[2]{command,use}:")
	fmt.Fprintln(w, "  webctl search <query> --goal <need>,find pages; work from the snippets")
	fmt.Fprintln(w, "  webctl fetch <url>... --goal <need>,read pages you already have the URLs for")

	fmt.Fprintf(w, "help[%d]:\n", len(help))
	for _, h := range help {
		fmt.Fprintf(w, "  %s\n", h)
	}
}

const (
	helpGoal   = "Always pass --goal; the relevance filter judges against it"
	helpScrape = "Add --scrape --filter-chunks to a search only when you would otherwise read a whole page"
	helpDocs   = "Run `webctl docs <topic>` for the full reference"
)

func contextHelp(canSummarize bool) []string {
	help := []string{helpGoal, helpScrape}
	if canSummarize {
		help = append(help, "Add --summarize when the kept text is still longer than you need; --summarize-model <name> picks the model for one call")
	}
	return append(help, helpDocs)
}

// claudeContextHelp states what Claude Code's permission check does with a
// webctl command, then the flags that make each shell addition unnecessary:
// agents piped into head or cut to shorten output, added 2>&1 out of habit,
// and ran background jobs into files to look up several things at once.
// The lines are facts rather than orders because Claude Code may surface
// imperative hook context to the user instead of using it.
func claudeContextHelp(canSummarize bool) []string {
	shorten := "narrow --goal, then lower --max-output"
	if canSummarize {
		shorten = "narrow --goal, then add --summarize (--summarize-model <name> picks its model), then lower --max-output"
	}
	return []string{
		"A bare webctl command runs under the webctl allow rule. A pipe, 2>&1, redirect, cd or && prefix, loop, or background & puts it before the auto-mode classifier, which can deny it; stderr already reaches the tool result",
		helpGoal,
		fmt.Sprintf("Output is capped at %d chars by default. For less, %s; these replace head, tail, cut, sed, and grep", DefaultMaxOutput, shorten),
		"fetch reads every URL it is given concurrently; several searches are several bare calls in one message, not background jobs writing files",
		helpScrape,
		helpDocs,
	}
}

func jevState(cfg *config.Config) string {
	if cfg.Keys.Get(keys.Jev) == "" {
		return "missing; fetch fails and search needs --no-filter until " + keys.Jev.EnvVarsLabel() + " is set"
	}
	if cfg.KeySource[keys.Jev] == "env" {
		return "configured via " + keys.Jev.EnvVarInUse()
	}
	return "configured"
}

// searchState lists the providers a search will try, in order, and warns
// when any of them is a hosted keyless endpoint, since those throttle.
func searchState(cfg *config.Config) string {
	chain, err := cfg.Chain("")
	if err != nil {
		return "unavailable; " + err.Error()
	}
	state := strings.Join(chain, ", ")
	for _, name := range chain {
		if slices.Contains(provider.KeylessChain(), name) && cfg.Keys.Get(keys.Name(name)) == "" {
			return state + " (keyless endpoints throttle by IP after a few dozen searches a day)"
		}
	}
	return state
}

func coolingState(cooling map[string]provider.CooldownEntry, now time.Time) string {
	var parts []string
	for name, e := range cooling {
		if now.Before(e.Until) {
			parts = append(parts, name+" for "+provider.FormatDuration(e.Until.Sub(now)))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// summarizeState names the backend and model --summarize would use, and
// whether it would work: a run starts with summarize.New, so a setting it
// rejects is reported here instead of advertised.
func summarizeState(sc summarize.Config) (string, bool) {
	if _, err := summarize.New(sc); err != nil {
		return "unavailable; " + err.Error(), false
	}
	model := strings.TrimSpace(sc.Model)
	if sc.Backend() == summarize.BackendEndpoint {
		return "endpoint backend, model " + model, true
	}
	if model == "" {
		return "command backend, its own default model", true
	}
	return "command backend, model " + model, true
}

// toonValue quotes a scalar field value when TOON requires it: empty,
// padded, keyword- or number-like, starting with a hyphen, or containing a
// colon, quote, backslash, bracket, brace, or control character.
func toonValue(s string) string {
	needs := s == "" || s != strings.TrimSpace(s) ||
		s == "true" || s == "false" || s == "null" ||
		strings.HasPrefix(s, "-") ||
		strings.ContainsAny(s, ":\"\\[]{}\n\r\t")
	if !needs {
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return s
		}
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}
