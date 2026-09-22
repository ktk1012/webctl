// Package config resolves runtime configuration for webctl.
//
// Precedence (highest first):
//  1. command-line flags (bound by the CLI layer)
//  2. environment variables (EXA_API_KEY, JEV_API_KEY, SEARXNG_URL, WEBCTL_PROVIDER, ...)
//  3. ~/webctl/config.yaml (optional)
//  4. ~/secrets/keys.json (for API keys only; --keys-file, WEBCTL_KEYS_FILE,
//     or keys_file in config.yaml point elsewhere)
//  5. built-in defaults
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/dorkitude/webctl/internal/keys"
	"github.com/dorkitude/webctl/internal/provider"
	"github.com/dorkitude/webctl/internal/summarize"
)

// Defaults.
const (
	DefaultProvider = "" // auto: first usable in Chain order
	DefaultNum      = 20
	DefaultMinScore = 6.0 // jev.DefaultCut(4): "useful" or better on the 0–10 scale
	// DefaultSources is how many providers a search gathers from and fuses.
	DefaultSources  = 3
	DefaultJevURL   = "https://api.typesafe.ai"
	DefaultJevModel = "jev-latest"
	EnvPrefix       = "WEBCTL"
)

// Config is the fully-resolved configuration.
type Config struct {
	// Dir is the webctl home directory (~/webctl).
	Dir string
	// KeysPath is the path to keys.json.
	KeysPath string

	// Provider is the preferred search provider name, or "" for auto.
	Provider string
	// Num is the default number of results to request.
	Num int
	// MinScore is the default relevance cutoff.
	MinScore float64
	// MinResults, when > 0, backfills the kept set to this many.
	MinResults int
	// Sources is how many providers to query per search.
	Sources int

	// JevBaseURL and JevModel configure the Jev client.
	JevBaseURL string
	JevModel   string

	// Cooldown is the rate-limit backoff policy; CooldownPath is its state file.
	Cooldown     provider.CooldownConfig
	CooldownPath string

	// Summarize configures the optional page summarizer (--summarize).
	Summarize summarize.Config

	// Keys holds the resolved API keys and the SearXNG URL (env overrides file).
	Keys *keys.Store
	// KeySource records where each key came from ("env", "file", or "").
	KeySource map[keys.Name]string
}

// Options tweak how Load behaves. Zero value uses defaults.
type Options struct {
	// Dir overrides ~/webctl. Mostly for tests.
	Dir string
	// KeysPath overrides the keys file (default ~/secrets/keys.json).
	KeysPath string
	// Viper lets the caller supply a pre-configured instance (e.g. with flags bound).
	Viper *viper.Viper
}

// New returns a Viper instance pre-wired with webctl defaults and env bindings.
func New() *viper.Viper {
	v := viper.New()
	v.SetDefault("provider", DefaultProvider)
	v.SetDefault("num", DefaultNum)
	v.SetDefault("min_score", DefaultMinScore)
	v.SetDefault("min_results", 0)
	v.SetDefault("sources", DefaultSources)
	v.SetDefault("jev.base_url", DefaultJevURL)
	v.SetDefault("jev.model", DefaultJevModel)
	// searxng_url may also be set at the top level of config.yaml.
	v.SetDefault("searxng_url", "")
	// keys_file: WEBCTL_KEYS_FILE or config.yaml; "" means keys.DefaultPath().
	v.SetDefault("keys_file", "")
	// Rate-limit cooldowns; see provider.DefaultCooldown.
	v.SetDefault("cooldown.enabled", provider.DefaultCooldown.Enabled)
	v.SetDefault("cooldown.steps", durationStrings(provider.DefaultCooldown.Steps))
	v.SetDefault("cooldown.probe_interval", provider.DefaultCooldown.ProbeInterval.String())
	v.SetDefault("cooldown.quota_start", provider.DefaultCooldown.QuotaStart)
	// Page summarizer; see summarize.Config and `webctl docs summarize`.
	v.SetDefault("summarize.command", "")
	v.SetDefault("summarize.endpoint", "")
	v.SetDefault("summarize.model", "")
	v.SetDefault("summarize.api_key", "")
	v.SetDefault("summarize.api_key_env", "")
	v.SetDefault("summarize.max_tokens", summarize.DefaultMaxTokens)
	v.SetDefault("summarize.reasoning_effort", "none")
	v.SetDefault("summarize.timeout", summarize.DefaultTimeout.String())

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	// Keys use their conventional unprefixed env vars.
	for _, n := range keys.All {
		_ = v.BindEnv(append([]string{keyField(n)}, n.EnvVars()...)...)
	}
	return v
}

func keyField(n keys.Name) string { return "keys." + string(n) }

func durationStrings(ds []time.Duration) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}
	return out
}

// cooldownConfig reads cooldown.* from v. Steps accept a YAML list or a
// comma-separated string (the env form).
func cooldownConfig(v *viper.Viper) (provider.CooldownConfig, error) {
	cfg := provider.CooldownConfig{
		Enabled:    v.GetBool("cooldown.enabled"),
		QuotaStart: v.GetInt("cooldown.quota_start"),
	}
	var raw []string
	for _, s := range v.GetStringSlice("cooldown.steps") {
		raw = append(raw, strings.Split(s, ",")...)
	}
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("cooldown.steps: %q is not a positive duration", s)
		}
		cfg.Steps = append(cfg.Steps, d)
	}
	if len(cfg.Steps) == 0 {
		return cfg, errors.New("cooldown.steps must list at least one duration")
	}
	pi, err := time.ParseDuration(v.GetString("cooldown.probe_interval"))
	if err != nil || pi <= 0 {
		return cfg, fmt.Errorf("cooldown.probe_interval: %q is not a positive duration", v.GetString("cooldown.probe_interval"))
	}
	cfg.ProbeInterval = pi
	if cfg.QuotaStart < 1 {
		cfg.QuotaStart = 1
	}
	return cfg, nil
}

// Load resolves configuration from all sources.
func Load(opts Options) (*Config, error) {
	dir := opts.Dir
	if dir == "" {
		var err error
		if dir, err = keys.DefaultDir(); err != nil {
			return nil, err
		}
	}

	v := opts.Viper
	if v == nil {
		v = New()
	}

	// Optional config.yaml for persistent defaults.
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, "config.yaml"), err)
		}
	}

	keysPath := opts.KeysPath
	if keysPath == "" {
		keysPath = strings.TrimSpace(v.GetString("keys_file"))
	}
	if keysPath == "" {
		p, err := keys.DefaultPath()
		if err != nil {
			return nil, err
		}
		keysPath = p
	}
	store, err := keys.Load(keysPath)
	if err != nil {
		return nil, err
	}

	cooldown, err := cooldownConfig(v)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Dir:          dir,
		KeysPath:     keysPath,
		Cooldown:     cooldown,
		CooldownPath: filepath.Join(dir, "cooldown.json"),
		Provider:     strings.ToLower(strings.TrimSpace(v.GetString("provider"))),
		Num:          v.GetInt("num"),
		MinScore:     v.GetFloat64("min_score"),
		MinResults:   v.GetInt("min_results"),
		Sources:      v.GetInt("sources"),
		JevBaseURL:   strings.TrimRight(v.GetString("jev.base_url"), "/"),
		JevModel:     v.GetString("jev.model"),
		Summarize:    summarizeConfig(v),
		Keys:         &keys.Store{},
		KeySource:    map[keys.Name]string{},
	}

	// Env (via viper) wins over the file.
	for _, n := range keys.All {
		if val := strings.TrimSpace(v.GetString(keyField(n))); val != "" {
			cfg.Keys.Set(n, val)
			cfg.KeySource[n] = "env"
			continue
		}
		if val := store.Get(n); val != "" {
			cfg.Keys.Set(n, val)
			cfg.KeySource[n] = "file"
		}
	}
	// searxng_url in config.yaml fills the slot when neither env nor keys.json set it.
	if !cfg.Keys.Has(keys.SearXNG) {
		if val := strings.TrimSpace(v.GetString("searxng_url")); val != "" {
			cfg.Keys.Set(keys.SearXNG, val)
			cfg.KeySource[keys.SearXNG] = "config"
		}
	}

	if cfg.Num <= 0 {
		return nil, fmt.Errorf("num must be positive, got %d", cfg.Num)
	}
	if cfg.Sources <= 0 {
		return nil, fmt.Errorf("sources must be positive, got %d", cfg.Sources)
	}
	return cfg, nil
}

// summarizeConfig reads summarize.* settings. The API key comes from
// summarize.api_key, or the environment variable named by
// summarize.api_key_env, or FIREWORKS_API_KEY / OPENAI_API_KEY when the
// endpoint host suggests one.
func summarizeConfig(v *viper.Viper) summarize.Config {
	c := summarize.Config{
		Command:         strings.TrimSpace(v.GetString("summarize.command")),
		Endpoint:        strings.TrimSpace(v.GetString("summarize.endpoint")),
		Model:           strings.TrimSpace(v.GetString("summarize.model")),
		APIKey:          strings.TrimSpace(v.GetString("summarize.api_key")),
		MaxTokens:       v.GetInt("summarize.max_tokens"),
		ReasoningEffort: strings.TrimSpace(v.GetString("summarize.reasoning_effort")),
	}
	if d, err := time.ParseDuration(strings.TrimSpace(v.GetString("summarize.timeout"))); err == nil && d > 0 {
		c.Timeout = d
	}
	if c.APIKey == "" {
		envName := strings.TrimSpace(v.GetString("summarize.api_key_env"))
		if envName == "" {
			switch {
			case strings.Contains(c.Endpoint, "fireworks.ai"):
				envName = "FIREWORKS_API_KEY"
			case strings.Contains(c.Endpoint, "openai.com"):
				envName = "OPENAI_API_KEY"
			case strings.Contains(c.Endpoint, "anthropic.com"):
				envName = "ANTHROPIC_API_KEY"
			}
		}
		if envName != "" {
			c.APIKey = strings.TrimSpace(os.Getenv(envName))
		}
	}
	return c
}

// ProviderKey returns the credential for the named search provider: the API
// key for keyed providers, the instance URL for searxng, and "" for ddg. Exa
// and Parallel yield "" without error when no key is set, since they work
// keyless. The error is actionable when a required credential is missing.
func (c *Config) ProviderKey(name string) (string, error) {
	name = provider.Normalize(name)
	if name == "ddg" || name == "ketch" {
		return "", nil
	}
	n, err := keys.Parse(name)
	if err != nil || n == keys.Jev {
		return "", fmt.Errorf("unknown provider %q (expected one of %s)", name, strings.Join(provider.Names(), ", "))
	}
	key := c.Keys.Get(n)
	if key == "" {
		if provider.Keyless(name) && !n.IsURL() {
			return "", nil
		}
		if n.IsURL() {
			return "", fmt.Errorf("no %s configured: run `webctl setup` or set %s", n.Display(), n.EnvVar())
		}
		return "", fmt.Errorf("no %s API key configured: run `webctl setup` or set %s", n.Display(), n.EnvVar())
	}
	return key, nil
}

// Usable reports whether the named provider can be constructed with the
// current configuration.
func (c *Config) Usable(name string) bool {
	_, err := c.ProviderKey(name)
	return err == nil
}

// JevKey returns the Jev API key, with an actionable error if it's missing.
// The caller adds any advice specific to its own flags: --no-filter is one
// way out of this for a search and no way out at all for a fetch.
func (c *Config) JevKey() (string, error) {
	key := c.Keys.Get(keys.Jev)
	if key == "" {
		return "", fmt.Errorf("no Jev API key configured: run `webctl setup` or set %s", keys.Jev.EnvVarsLabel())
	}
	return key, nil
}

// Chain returns the providers to try, in order. An explicit choice yields a
// one-element chain (validated later by ProviderKey). Otherwise the preferred
// provider from config comes first (an error if it is unusable, since the
// user asked for it), then searxng and degoog when their URLs are set,
// then every provider with a key in Keyed order. With no key at all the
// chain is provider.KeylessChain: the hosted keyless endpoints, then ddg.
// Setting a key is a choice of engine, so the free tiers leave the chain
// as soon as one exists.
func (c *Config) Chain(explicit string) ([]string, error) {
	if explicit = provider.Normalize(explicit); explicit != "" {
		return []string{explicit}, nil
	}
	var chain []string
	add := func(name string) {
		for _, have := range chain {
			if have == name {
				return
			}
		}
		chain = append(chain, name)
	}
	if pref := provider.Normalize(c.Provider); pref != "" {
		if _, err := c.ProviderKey(pref); err != nil {
			return nil, fmt.Errorf("configured provider %q is unusable: %w", pref, err)
		}
		add(pref)
	}
	// Your own metasearch first: no quota, no cost.
	for _, own := range []string{"searxng", "degoog"} {
		if c.Usable(own) {
			add(own)
		}
	}
	// Providers you set a key for. Configuring a key is a choice of
	// engine, so once any key exists the free tiers stay out of the chain.
	keyed := 0
	for _, name := range provider.Keyed() {
		if c.Keys.Get(keys.Name(name)) != "" {
			add(name)
			keyed++
		}
	}
	if keyed == 0 {
		// Nothing configured: the hosted keyless endpoints directly, with
		// DuckDuckGo as the last resort.
		for _, name := range provider.KeylessChain() {
			add(name)
		}
	}
	return chain, nil
}

// ResolveProvider returns the first provider in Chain(explicit).
func (c *Config) ResolveProvider(explicit string) (string, error) {
	chain, err := c.Chain(explicit)
	if err != nil {
		return "", err
	}
	return chain[0], nil
}
