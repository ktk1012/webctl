// Package cli wires up the webctl cobra commands.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/dorkitude/webctl/internal/config"
	version_ "github.com/dorkitude/webctl/internal/version"
)

var (
	// version is set at build time via -ldflags "-X .../cli.version=v1.2.3";
	// otherwise the behavior version is reported.
	version = version_.Version

	// configDir overrides ~/webctl when set via --config-dir.
	configDir string
	// keysFile overrides ~/secrets/keys.json when set via --keys-file.
	keysFile string

	// v is the shared viper instance; flags are bound into it per command.
	v *viper.Viper
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "webctl [flags] <query>",
		Short: "Web search qualified by Jev's typed relevance scoring",
		Long: `Searches several web providers at once, folds duplicates, and has Jev
(TypeSafe's System One model) score every result for topic and source
quality, so only results worth reading reach your context window.

Required: a Jev key (webctl setup). A search key is strongly advised
(Brave: 5,000 free searches a month). Without one, search uses the
keyless Exa, Parallel, Keenable, You.com, and Firecrawl endpoints, then
DuckDuckGo; these throttle by IP, and throttled providers back off (see
"cooldown").

ALWAYS pass --goal. It is the single biggest lever on result quality:
the query goes to the engines, the goal goes to every Jev judge next to
it, and without a goal the judges can only guess what you are after.

  webctl search "<query>" --goal "<what you actually need>"

Snippets are usually enough. Whenever you would otherwise read a whole
page, add --scrape --filter-chunks instead: webctl fetches the top
results and returns only the chunks Jev finds relevant, so a long
thread, PDF, or article costs a fraction of the tokens.

Already have the URL? "webctl fetch <url> --goal ..." does the same to
a page you name, without running a search.

Help text is short by design. The full reference is compiled in:
  webctl docs            topics
  webctl docs <topic>    one page (search, providers, config, ...)
  webctl docs all        everything`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runSearch(cmd, args)
		},
	}
	root.PersistentFlags().StringVar(&configDir, "config-dir", "", "config directory (default ~/webctl)")
	root.PersistentFlags().StringVar(&keysFile, "keys-file", "", "keys file (default ~/secrets/keys.json)")

	addSearchFlags(root)
	search := &cobra.Command{
		Use:   "search [flags] <query>",
		Short: "Search the web and keep only results Jev judges worth reading",
		Long: `Always pass --goal; it is highly recommended on every search. The query goes
to the search engines; the goal tells the judges what you are actually after.
Both reach every Jev judgment as "Query:" and "Goal:" lines, and without a
goal the judges score against the query alone, which keeps more noise.

  webctl search "final score san francisco giants september 19th baseball" \
    --goal "The score of the Giants game on the night of September 19th."

A bare "webctl <query>" runs the same pipeline without a goal; use it only
when the query already says everything the judges need.

Work from the snippets when they answer the question. Any time you are
about to read a whole page, add --scrape --filter-chunks instead: only the
chunks Jev judges relevant to the goal are returned.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runSearch(cmd, args)
		},
	}
	addSearchFlags(search)
	root.AddCommand(search)
	root.AddCommand(newFetchCmd())
	root.AddCommand(newSetupCmd())
	root.AddCommand(newKeysCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newCooldownCmd())
	root.AddCommand(newDocsCmd())
	root.AddCommand(newEvalCmd())
	return root
}

// Execute runs the CLI and prints any error to stderr.
func Execute() error {
	v = config.New()
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return err
	}
	return nil
}

// loadConfig resolves configuration, honoring --config-dir and --keys-file.
func loadConfig() (*config.Config, error) {
	return config.Load(config.Options{Dir: configDir, KeysPath: keysFile, Viper: v})
}
