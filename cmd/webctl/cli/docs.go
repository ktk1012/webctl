package cli

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dorkitude/webctl/docs"
)

// docTopics is the reading order for `docs all`; anything else in the
// embedded set follows alphabetically.
var docTopics = []string{"index", "search", "fetch", "providers", "cooldowns", "filtering", "scraping", "dedupe", "config", "evals", "searxng"}

// docSummary returns the first sentence after the title of a page.
func docSummary(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if i == 0 || strings.TrimSpace(l) == "" {
			continue
		}
		s := strings.TrimSpace(l)
		if j := strings.Index(s, ". "); j > 0 {
			s = s[:j+1]
		}
		if r := []rune(s); len(r) > 100 {
			s = strings.TrimSpace(string(r[:99])) + "…"
		}
		return s
	}
	return ""
}

func docNames() ([]string, error) {
	entries, err := fs.ReadDir(docs.FS, ".")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			seen[strings.TrimSuffix(e.Name(), ".md")] = true
		}
	}
	for _, t := range docTopics {
		if seen[t] {
			names = append(names, t)
			delete(seen, t)
		}
	}
	var rest []string
	for n := range seen {
		rest = append(rest, n)
	}
	sort.Strings(rest)
	return append(names, rest...), nil
}

func newDocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs [topic|all]",
		Short: "Print the full reference (terse, complete); --help is the short form",
		Long: `The --help text on every command is kept short to save tokens. The full
reference is compiled into this binary: run "docs" for the topic list, "docs
<topic>" for one page, "docs all" for everything in reading order.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := docNames()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				fmt.Fprintln(out, "Topics (webctl docs <topic>, or `docs all`):")
				for _, n := range names {
					body, _ := fs.ReadFile(docs.FS, n+".md")
					fmt.Fprintf(out, "  %-14s %s\n", n, docSummary(string(body)))
				}
				return nil
			}
			want := strings.ToLower(strings.TrimSpace(args[0]))
			if want == "all" {
				for i, n := range names {
					body, _ := fs.ReadFile(docs.FS, n+".md")
					if i > 0 {
						fmt.Fprint(out, "\n---\n\n")
					}
					fmt.Fprint(out, string(body))
				}
				return nil
			}
			body, err := fs.ReadFile(docs.FS, want+".md")
			if err != nil {
				body, err = fs.ReadFile(docs.FS, strings.ToUpper(want)+".md")
			}
			if err != nil {
				return fmt.Errorf("no doc %q (topics: %s)", want, strings.Join(names, ", "))
			}
			fmt.Fprint(out, string(body))
			return nil
		},
	}
	return cmd
}
