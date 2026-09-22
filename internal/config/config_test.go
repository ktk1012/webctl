package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorkitude/webctl/internal/keys"
)

// clearEnv unsets every env var that could leak into a Load call.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, n := range keys.All {
		for _, env := range n.EnvVars() {
			t.Setenv(env, "")
		}
	}
	for _, k := range []string{"PROVIDER", "NUM", "MIN_SCORE", "JEV_BASE_URL", "JEV_MODEL", "SOURCES", "MIN_RESULTS", "COOLDOWN_ENABLED", "COOLDOWN_STEPS"} {
		t.Setenv(EnvPrefix+"_"+k, "")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != dir || cfg.KeysPath != filepath.Join(dir, "keys.json") {
		t.Errorf("paths = %q %q", cfg.Dir, cfg.KeysPath)
	}
	if cfg.Provider != DefaultProvider || cfg.Num != DefaultNum || cfg.MinScore != DefaultMinScore {
		t.Errorf("defaults = %q %d %v", cfg.Provider, cfg.Num, cfg.MinScore)
	}
	if cfg.JevBaseURL != DefaultJevURL || cfg.JevModel != DefaultJevModel {
		t.Errorf("jev defaults = %q %q", cfg.JevBaseURL, cfg.JevModel)
	}
	if len(cfg.Keys.ConfiguredProviders()) != 0 || len(cfg.KeySource) != 0 {
		t.Errorf("expected no keys, got %+v %v", cfg.Keys, cfg.KeySource)
	}
}

func TestLoadDefaultDir(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != filepath.Join(home, "webctl") || cfg.KeysPath != filepath.Join(home, "secrets", "keys.json") {
		t.Errorf("paths = %q %q", cfg.Dir, cfg.KeysPath)
	}
}

func TestLoadKeysFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keys.json"), `{"exa_api_key":"exa-file","jev_api_key":"jev-file","sonar_api_key":""}`)
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys.Get(keys.Exa) != "exa-file" || cfg.KeySource[keys.Exa] != "file" {
		t.Errorf("exa = %q from %q", cfg.Keys.Get(keys.Exa), cfg.KeySource[keys.Exa])
	}
	if cfg.Keys.Get(keys.Jev) != "jev-file" || cfg.KeySource[keys.Jev] != "file" {
		t.Errorf("jev = %q from %q", cfg.Keys.Get(keys.Jev), cfg.KeySource[keys.Jev])
	}
	if cfg.Keys.Has(keys.Sonar) || cfg.KeySource[keys.Sonar] != "" {
		t.Errorf("sonar should be unset, got %q", cfg.KeySource[keys.Sonar])
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keys.json"), `{"exa_api_key":"exa-file"}`)
	t.Setenv("EXA_API_KEY", "  exa-env  ")
	t.Setenv("PARALLEL_API_KEY", "par-env")
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys.Get(keys.Exa) != "exa-env" || cfg.KeySource[keys.Exa] != "env" {
		t.Errorf("exa = %q from %q, want trimmed env value", cfg.Keys.Get(keys.Exa), cfg.KeySource[keys.Exa])
	}
	if cfg.Keys.Get(keys.Parallel) != "par-env" || cfg.KeySource[keys.Parallel] != "env" {
		t.Errorf("parallel = %q from %q", cfg.Keys.Get(keys.Parallel), cfg.KeySource[keys.Parallel])
	}
}

func TestLoadConfigYAML(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
provider: " Sonar "
num: 25
min_score: 2
jev:
  base_url: https://jev.example/
  model: jev-2
`)
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "sonar" {
		t.Errorf("provider = %q (should be lowercased and trimmed)", cfg.Provider)
	}
	if cfg.Num != 25 || cfg.MinScore != 2 {
		t.Errorf("num=%d min_score=%v", cfg.Num, cfg.MinScore)
	}
	if cfg.JevBaseURL != "https://jev.example" || cfg.JevModel != "jev-2" {
		t.Errorf("jev = %q %q (trailing slash should be trimmed)", cfg.JevBaseURL, cfg.JevModel)
	}
}

func TestLoadEnvOverridesConfigYAML(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "provider: exa\nnum: 5\n")
	t.Setenv("WEBCTL_PROVIDER", "parallel")
	t.Setenv("WEBCTL_NUM", "7")
	t.Setenv("WEBCTL_JEV_MODEL", "jev-env")
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "parallel" || cfg.Num != 7 || cfg.JevModel != "jev-env" {
		t.Errorf("cfg = %q %d %q", cfg.Provider, cfg.Num, cfg.JevModel)
	}
}

func TestLoadBadConfigYAML(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "provider: [unclosed\n")
	if _, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")}); err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadBadKeysJSON(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keys.json"), "{oops")
	if _, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")}); err == nil || !strings.Contains(err.Error(), "keys.json") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadRejectsNonPositiveNum(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "num: 0\n")
	if _, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")}); err == nil || !strings.Contains(err.Error(), "num must be positive") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadWithSuppliedViper(t *testing.T) {
	clearEnv(t)
	v := New()
	v.Set("num", 3) // simulates a bound flag
	cfg, err := Load(Options{Dir: t.TempDir(), Viper: v})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Num != 3 {
		t.Errorf("num = %d, want 3 from supplied viper", cfg.Num)
	}
}

func TestProviderKey(t *testing.T) {
	cfg := &Config{Keys: &keys.Store{ExaAPIKey: "x"}}
	if k, err := cfg.ProviderKey("exa"); err != nil || k != "x" {
		t.Errorf("exa = %q, %v", k, err)
	}
	if _, err := cfg.ProviderKey("sonar"); err == nil || !strings.Contains(err.Error(), "SONAR_API_KEY") {
		t.Errorf("missing sonar key err = %v", err)
	}
	if _, err := cfg.ProviderKey("jev"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("jev is not a provider: %v", err)
	}
	if _, err := cfg.ProviderKey("bing"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("bing err = %v", err)
	}
	if k, err := cfg.ProviderKey("ddg"); err != nil || k != "" {
		t.Errorf("ddg needs no key: %q, %v", k, err)
	}
	if _, err := cfg.ProviderKey("searxng"); err == nil || !strings.Contains(err.Error(), "SEARXNG_URL") {
		t.Errorf("missing searxng url err = %v", err)
	}
	cfg.Keys.Set(keys.SearXNG, "http://sx:8080")
	if k, err := cfg.ProviderKey("searxng"); err != nil || k != "http://sx:8080" {
		t.Errorf("searxng = %q, %v", k, err)
	}
	if !cfg.Usable("ddg") || !cfg.Usable("exa") || !cfg.Usable("searxng") || cfg.Usable("sonar") || cfg.Usable("bing") {
		t.Error("Usable mismatch")
	}
	// Exa and Parallel are usable keyless; Sonar is not.
	if k, err := (&Config{Keys: &keys.Store{}}).ProviderKey("parallel"); err != nil || k != "" {
		t.Errorf("keyless parallel = %q, %v", k, err)
	}
}

func TestLoadSearXNGURLFromConfigYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEARXNG_URL", "")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("searxng_url: http://sx.local:8080/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Keys.Get(keys.SearXNG); got != "http://sx.local:8080/" || cfg.KeySource[keys.SearXNG] != "config" {
		t.Errorf("searxng from yaml = %q (%s)", got, cfg.KeySource[keys.SearXNG])
	}
	t.Setenv("SEARXNG_URL", "http://env:1")
	cfg, err = Load(Options{Dir: dir, KeysPath: filepath.Join(dir, "keys.json")})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Keys.Get(keys.SearXNG); got != "http://env:1" || cfg.KeySource[keys.SearXNG] != "env" {
		t.Errorf("env should beat yaml: %q (%s)", got, cfg.KeySource[keys.SearXNG])
	}
}

func TestJevKey(t *testing.T) {
	cfg := &Config{Keys: &keys.Store{}}
	if _, err := cfg.JevKey(); err == nil || !strings.Contains(err.Error(), "webctl setup") {
		t.Errorf("missing jev key should say how to configure one: %v", err)
	}
	cfg.Keys.Set(keys.Jev, "j")
	if k, err := cfg.JevKey(); err != nil || k != "j" {
		t.Errorf("jev = %q, %v", k, err)
	}
}

// The Jev key is also read from TYPESAFE_API_KEY, the variable TypeSafe's own
// SDKs use, so a machine already set up for them needs no second copy of the
// secret. JEV_API_KEY stays canonical and wins when both are set.
func TestJevKeyFromTypeSafeEnv(t *testing.T) {
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.json")

	clearEnv(t)
	t.Setenv("TYPESAFE_API_KEY", "ts")
	cfg, err := Load(Options{Dir: dir, KeysPath: keysPath})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.JevKey(); err != nil || got != "ts" {
		t.Errorf("jev = %q, %v; want ts", got, err)
	}
	if cfg.KeySource[keys.Jev] != "env" {
		t.Errorf("KeySource = %q, want env", cfg.KeySource[keys.Jev])
	}

	t.Setenv("JEV_API_KEY", "jev")
	cfg, err = Load(Options{Dir: dir, KeysPath: keysPath})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := cfg.JevKey(); got != "jev" {
		t.Errorf("jev = %q; want the canonical variable to win", got)
	}
}

func TestChain(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		explicit string
		want     []string
		wantErr  bool
	}{
		{"explicit wins", Config{Provider: "exa", Keys: &keys.Store{SonarAPIKey: "s"}}, "Sonar", []string{"sonar"}, false},
		{"explicit even without key", Config{Provider: "exa", Keys: &keys.Store{}}, "parallel", []string{"parallel"}, false},
		{"explicit alias", Config{Keys: &keys.Store{}}, "DuckDuckGo", []string{"ddg"}, false},
		{"preferred first, then keyed in order, then ddg", Config{Provider: "sonar", Keys: &keys.Store{ParallelAPIKey: "p", ExaAPIKey: "e", SonarAPIKey: "s"}}, "", []string{"sonar", "exa", "parallel"}, false},
		{"preferred without key is an error", Config{Provider: "sonar", Keys: &keys.Store{ExaAPIKey: "e"}}, "", nil, true},
		{"no keys: keyless endpoints then ddg", Config{Keys: &keys.Store{}}, "", []string{"parallel", "exa", "keenable", "youcom", "firecrawl", "ddg"}, false},
		{"searxng first when configured", Config{Keys: &keys.Store{SearXNGURL: "http://sx", ExaAPIKey: "e"}}, "", []string{"searxng", "exa"}, false},
		{"preferred keyless", Config{Provider: "searxng", Keys: &keys.Store{SearXNGURL: "http://sx", ExaAPIKey: "e"}}, "", []string{"searxng", "exa"}, false},
		{"preferred ddg", Config{Provider: "ddg", Keys: &keys.Store{ExaAPIKey: "e"}}, "", []string{"ddg", "exa"}, false},
	}
	for _, tc := range cases {
		got, err := tc.cfg.Chain(tc.explicit)
		if (err != nil) != tc.wantErr || strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: got %v, %v; want %v, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
		first, err2 := tc.cfg.ResolveProvider(tc.explicit)
		if (err2 != nil) != tc.wantErr || (len(tc.want) > 0 && first != tc.want[0]) {
			t.Errorf("%s: ResolveProvider = %q, %v", tc.name, first, err2)
		}
	}
}

func TestKeysFileEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "elsewhere.json")
	writeFile(t, path, `{"jev_api_key":"from-elsewhere"}`)
	t.Setenv(EnvPrefix+"_KEYS_FILE", path)
	cfg, err := Load(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeysPath != path || cfg.Keys.Get(keys.Jev) != "from-elsewhere" {
		t.Errorf("KeysPath = %q, jev = %q", cfg.KeysPath, cfg.Keys.Get(keys.Jev))
	}
}
