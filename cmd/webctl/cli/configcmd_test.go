package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorkitude/webctl/internal/config"
	"github.com/dorkitude/webctl/internal/keys"
)

func runConfig(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	v = config.New()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--config-dir", dir, "--keys-file", filepath.Join(dir, "keys.json")}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestConfigSetGetShowUnset(t *testing.T) {
	for _, n := range keys.All {
		for _, env := range n.EnvVars() {
			t.Setenv(env, "")
		}
	}
	for _, k := range []string{"PROVIDER", "NUM", "MIN_SCORE", "JEV_BASE_URL", "JEV_MODEL", "KEYS_FILE", "SEARXNG_URL", "COOLDOWN_ENABLED", "COOLDOWN_STEPS", "SOURCES", "MIN_RESULTS"} {
		t.Setenv(config.EnvPrefix+"_"+k, "")
	}
	dir := t.TempDir()

	if out, err := runConfig(t, dir, "config", "set", "provider", "Parallel"); err != nil || !strings.Contains(out, "Set provider = parallel") {
		t.Fatalf("set provider: %q %v", out, err)
	}
	if _, err := runConfig(t, dir, "config", "set", "provider", "bing"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("bad provider should fail: %v", err)
	}
	if _, err := runConfig(t, dir, "config", "set", "num", "0"); err == nil {
		t.Error("num 0 should fail")
	}
	if _, err := runConfig(t, dir, "config", "set", "jev.model", "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := runConfig(t, dir, "config", "set", "min_score", "2.2"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	for _, want := range []string{"provider: parallel", "model: jev-1.13.0", "min_score: 2.2"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.yaml missing %q:\n%s", want, data)
		}
	}

	out, err := runConfig(t, dir, "config", "get", "jev.model")
	if err != nil || strings.TrimSpace(out) != "jev-1.13.0" {
		t.Errorf("get = %q, %v", out, err)
	}
	t.Setenv("WEBCTL_NUM", "7")
	out, err = runConfig(t, dir, "config", "show")
	if err != nil || !strings.Contains(out, "config.yaml") || !strings.Contains(out, "env WEBCTL_NUM") || !strings.Contains(out, "default") {
		t.Errorf("show = %q, %v", out, err)
	}
	if out, err := runConfig(t, dir, "config", "unset", "provider"); err != nil || !strings.Contains(out, "Removed provider") {
		t.Errorf("unset = %q, %v", out, err)
	}
	if out, _ := runConfig(t, dir, "config", "unset", "provider"); !strings.Contains(out, "was not set") {
		t.Errorf("second unset = %q", out)
	}
	if _, err := runConfig(t, dir, "config", "get", "bogus"); err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Errorf("bogus setting: %v", err)
	}
	if _, err := runConfig(t, dir, "config", "set", "cooldown.steps", "5m,1h,bogus"); err == nil {
		t.Error("bad step should fail")
	}
	if _, err := runConfig(t, dir, "config", "set", "cooldown.steps", "5m,1h"); err != nil {
		t.Fatal(err)
	}
	out, err = runConfig(t, dir, "config", "get", "cooldown.steps")
	if err != nil || !strings.Contains(out, "5m") {
		t.Errorf("cooldown.steps get = %q, %v", out, err)
	}
}
