package summarize

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var sample = Input{
	Query: "redis xautoclaim", Goal: "which version introduced XAUTOCLAIM",
	Title: "XAUTOCLAIM | Docs", URL: "https://redis.io/docs/latest/commands/xautoclaim/",
	Chunks: []string{"Available since: Redis 6.2.0.", "Time complexity: O(1) if COUNT is small."},
}

func TestCommandBackendGetsPromptOnStdinAndReturnsStdout(t *testing.T) {
	// The command echoes the prompt back with a marker, so the test can see
	// both that the prompt arrived and that stdout is the summary.
	s, err := New(Config{Command: "sed -n '1p;/^Goal:/p;/^URL:/p' | tr '\\n' '|'"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Name(), "command: ") {
		t.Errorf("name = %q", s.Name())
	}
	out, usage, err := s.Summarize(context.Background(), sample)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Goal: which version introduced XAUTOCLAIM") || !strings.Contains(out, "URL: https://redis.io") {
		t.Errorf("prompt did not reach the command or its output was not returned: %q", out)
	}
	if usage != (Usage{}) {
		t.Errorf("commands report no usage, got %+v", usage)
	}
}

// The configured model reaches a command as $WEBCTL_SUMMARIZE_MODEL, so one
// command line can serve several models; with none configured the command
// falls back to its own default, and the model given for this run wins over
// one exported in the environment.
func TestCommandBackendReceivesModel(t *testing.T) {
	const echoModel = `printf '%s' "${WEBCTL_SUMMARIZE_MODEL:-own-default}"`

	t.Setenv(ModelEnv, "")
	s, _ := New(Config{Command: echoModel})
	if out, _, err := s.Summarize(context.Background(), sample); err != nil || out != "own-default" {
		t.Errorf("no model: got %q, %v; want the command's own default", out, err)
	}

	s, _ = New(Config{Command: echoModel, Model: "haiku"})
	if out, _, err := s.Summarize(context.Background(), sample); err != nil || out != "haiku" {
		t.Errorf("model haiku: got %q, %v", out, err)
	}

	t.Setenv(ModelEnv, "exported")
	s, _ = New(Config{Command: echoModel, Model: "sonnet"})
	if out, _, err := s.Summarize(context.Background(), sample); err != nil || out != "sonnet" {
		t.Errorf("the run's model should beat the exported one: got %q, %v", out, err)
	}
}

func TestCommandBackendFailures(t *testing.T) {
	s, _ := New(Config{Command: "echo boom >&2; exit 3"})
	if _, _, err := s.Summarize(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("stderr should be in the error: %v", err)
	}
	s, _ = New(Config{Command: "true"})
	if _, _, err := s.Summarize(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("empty stdout should error: %v", err)
	}
	s, _ = New(Config{Command: "sleep 5", Timeout: 100 * time.Millisecond})
	start := time.Now()
	if _, _, err := s.Summarize(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout did not cut the command short")
	}
}

func TestEndpointBackend(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path "+r.URL.Path, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "```\nRedis 6.2.0 introduced it.\n```"}}},
			"usage":   map[string]int{"prompt_tokens": 120, "completion_tokens": 9},
		})
	}))
	defer srv.Close()
	s, err := New(Config{Endpoint: srv.URL + "/v1", Model: "tiny", APIKey: "k", ReasoningEffort: "none"})
	if err != nil {
		t.Fatal(err)
	}
	out, usage, err := s.Summarize(context.Background(), sample)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Redis 6.2.0 introduced it." {
		t.Errorf("out = %q (code fence should be stripped)", out)
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 9 {
		t.Errorf("usage = %+v", usage)
	}
	if gotAuth != "Bearer k" || gotBody["model"] != "tiny" || gotBody["reasoning_effort"] != "none" || gotBody["max_tokens"].(float64) != DefaultMaxTokens {
		t.Errorf("request = auth %q body %+v", gotAuth, gotBody)
	}
	msgs := gotBody["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "Goal: which version introduced XAUTOCLAIM") || !strings.Contains(content, "Available since: Redis 6.2.0.") {
		t.Errorf("prompt = %q", content)
	}
	if calls != 1 {
		t.Errorf("calls = %d", calls)
	}
}

// A server that rejects reasoning_effort gets one retry without it.
func TestEndpointDropsReasoningEffortOn400(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["reasoning_effort"]; has {
			http.Error(w, `{"error":"unknown field reasoning_effort"}`, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}}})
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL + "/chat/completions", Model: "m", ReasoningEffort: "none"})
	out, _, err := s.Summarize(context.Background(), sample)
	if err != nil || out != "ok" || calls != 2 {
		t.Errorf("out=%q err=%v calls=%d", out, err, calls)
	}
}

func TestEndpointErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"quota"}`, 429)
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL, Model: "m"})
	if _, _, err := s.Summarize(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("err = %v", err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": ""}}}, "usage": map[string]int{"completion_tokens": 500}})
	}))
	defer empty.Close()
	s, _ = New(Config{Endpoint: empty.URL, Model: "m"})
	if _, _, err := s.Summarize(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "empty message") {
		t.Errorf("empty content should explain reasoning budgets: %v", err)
	}
}

func TestNewRequiresABackend(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("nothing configured should error")
	}
	if _, err := New(Config{Endpoint: "https://x"}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("endpoint without model: %v", err)
	}
	if !(Config{Command: "x"}).Configured() || (Config{}).Configured() {
		t.Error("Configured")
	}
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, ""},
		{Config{Command: "x"}, BackendCommand},
		{Config{Endpoint: "https://x", Model: "m"}, BackendEndpoint},
		{Config{Command: "x", Endpoint: "https://x", Model: "m"}, BackendCommand},
		{Config{Command: "  "}, ""},
	} {
		if got := tc.cfg.Backend(); got != tc.want {
			t.Errorf("Backend(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestIsNothing(t *testing.T) {
	for _, s := range []string{"NOTHING RELEVANT", "nothing relevant.", "  **Nothing Relevant**  "} {
		if !IsNothing(s) {
			t.Errorf("%q should be nothing", s)
		}
	}
	if IsNothing("Nothing relevant except the version: 6.2") {
		t.Error("a real summary is not nothing")
	}
}
