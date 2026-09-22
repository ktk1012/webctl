package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dorkitude/webctl/internal/config"
	"github.com/dorkitude/webctl/internal/keys"
)

func newSetupCmd() *cobra.Command {
	var noValidate bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactive wizard to configure and validate API keys",
		Long: `Walks you through the Jev key (required: Jev is the filter) and the optional
search keys (Exa, Parallel, Sonar, You.com) and SearXNG URL that lift the
keyless providers' rate limits.

Key input is masked, each entry is validated with a lightweight API call, and
settings are stored in ~/secrets/keys.json with mode 0600.

Re-run at any time to add, rotate, or re-validate keys.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return runSetup(cmd.Context(), cfg, !noValidate)
		},
	}
	cmd.Flags().BoolVar(&noValidate, "no-validate", false, "save keys without validating them against the API")
	return cmd
}

func runSetup(ctx context.Context, cfg *config.Config, validate bool) error {
	store, err := keys.Load(cfg.KeysPath)
	if err != nil {
		return err
	}

	fmt.Println("=== webctl setup ===")
	fmt.Println()
	fmt.Println("Required: a Jev key. A search key is strongly advised (Brave is free for")
	fmt.Println("5,000 searches a month). Without one, searches use the keyless hosted")
	fmt.Println("endpoints and DuckDuckGo, which throttle by IP.")
	fmt.Printf("Each key is written to %s as soon as it is accepted.\n", prettyPath(cfg.KeysPath))
	fmt.Println()

	// --- Search providers ---
	for {
		fmt.Println("Search providers (optional):")
		for i, n := range keys.SearchProviders {
			fmt.Printf("  %d) %-20s %s\n", i+1, n.Display(), statusLabel(cfg, store, n))
		}
		fmt.Printf("  -  %-20s %s\n", "DuckDuckGo (ddg)", "[always available, no key]")
		fmt.Println("  s) Skip to Jev key")
		fmt.Println()

		choice, err := keys.PromptLine("Choose a provider to configure: ")
		if err != nil {
			return abort(err)
		}
		choice = strings.ToLower(strings.TrimSpace(choice))
		if choice == "s" || choice == "skip" {
			break
		}
		if choice == "q" || choice == "quit" {
			return errors.New("setup aborted; keys accepted so far are saved")
		}
		n, ok := pickProvider(choice)
		if !ok {
			fmt.Printf("  Unrecognized choice %q. Enter 1-%d or s.\n\n", choice, len(keys.SearchProviders))
			continue
		}
		if err := configureKey(ctx, cfg, store, n, validate); err != nil {
			return abort(err)
		}
		if err := store.Save(cfg.KeysPath); err != nil {
			return err
		}
		fmt.Println()
	}

	if len(store.ConfiguredProviders()) == 0 && len(cfg.Keys.ConfiguredProviders()) == 0 {
		fmt.Println("  No search key configured; searches will use the keyless endpoints and DuckDuckGo (throttled by IP).")
	}
	fmt.Println()

	// --- Jev ---
	fmt.Printf("Jev (TypeSafe) configuration: %s\n", statusLabel(cfg, store, keys.Jev))
	fmt.Println("  Jev is the filter and is required; only --no-filter searches work without it.")
	if store.Has(keys.Jev) {
		ans, err := keys.PromptLine("  Replace existing Jev key? [y/N]: ")
		if err != nil {
			return abort(err)
		}
		if strings.HasPrefix(strings.ToLower(ans), "y") {
			if err := configureKey(ctx, cfg, store, keys.Jev, validate); err != nil {
				return abort(err)
			}
		}
	} else {
		if err := configureKey(ctx, cfg, store, keys.Jev, validate); err != nil {
			return abort(err)
		}
	}
	fmt.Println()

	if err := store.Save(cfg.KeysPath); err != nil {
		return err
	}
	fmt.Printf("Keys saved to %s\n", prettyPath(cfg.KeysPath))
	return nil
}

func pickProvider(choice string) (keys.Name, bool) {
	for i, n := range keys.SearchProviders {
		if choice == fmt.Sprint(i+1) || choice == string(n) {
			return n, true
		}
	}
	return "", false
}

// configureKey prompts for a key (or, for SearXNG, an instance URL),
// validates it, and stores it. Returns nil if the user chose to skip. Only
// ErrCancelled propagates as an error.
func configureKey(ctx context.Context, cfg *config.Config, store *keys.Store, n keys.Name, validate bool) error {
	for {
		var key string
		var err error
		if n.Secret() {
			key, err = keys.PromptMasked(fmt.Sprintf("  %s API key: ", n.Display()))
		} else {
			key, err = keys.PromptLine(fmt.Sprintf("  %s (e.g. http://localhost:8080): ", n.Display()))
			key = strings.TrimSpace(key)
		}
		if err != nil {
			return err
		}
		if key == "" {
			fmt.Println("  (empty — skipped)")
			return nil
		}
		if n.IsURL() && !strings.HasPrefix(key, "http://") && !strings.HasPrefix(key, "https://") {
			fmt.Printf("  ✗ The %s must start with http:// or https://\n", n.Display())
			continue
		}

		if !validate {
			store.Set(n, key)
			fmt.Println("  ✓ Key saved (validation skipped)")
			return nil
		}

		fmt.Print("  Validating… ")
		err = validateKey(ctx, cfg, n, key)
		if err == nil {
			fmt.Print("\r")
			fmt.Println("  ✓ Key validated successfully")
			store.Set(n, key)
			return nil
		}
		fmt.Print("\r")
		fmt.Printf("  ✗ Validation failed: %v\n", err)

		ans, err := keys.PromptLine("  [r]etry, [s]ave anyway, or s[k]ip? [r]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "s", "save":
			store.Set(n, key)
			fmt.Println("  ⚠ Saved unvalidated key")
			return nil
		case "k", "skip":
			return nil
		default:
			continue
		}
	}
}

func statusLabel(cfg *config.Config, store *keys.Store, n keys.Name) string {
	if cfg.KeySource[n] == "env" {
		return fmt.Sprintf("[set via %s]", n.EnvVarInUse())
	}
	if store.Has(n) {
		return "[configured " + keys.Mask(store.Get(n)) + "]"
	}
	if cfg.KeySource[n] == "config" {
		return "[set in config.yaml: " + cfg.Keys.Get(n) + "]"
	}
	return "[not set]"
}

func abort(err error) error {
	if errors.Is(err, keys.ErrCancelled) {
		return errors.New("setup cancelled; keys accepted so far are saved")
	}
	return err
}

func prettyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
