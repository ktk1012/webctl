// Package summarize condenses the relevant chunks of a scraped page into a
// short, goal-focused summary using a small general model. It is not Jev:
// Jev decides what is relevant; this step rewrites what survived so an
// agent reads a paragraph instead of a page.
//
// Two backends, chosen by configuration:
//
//   - Command: any CLI that reads a prompt on stdin and prints the summary
//     on stdout (claude -p, codex exec, pi -p, llm, ...). A configured
//     model reaches it as $WEBCTL_SUMMARIZE_MODEL.
//   - Endpoint: an OpenAI-compatible /chat/completions URL with a model
//     name and an API key (Fireworks, OpenAI, Anthropic's compatibility
//     endpoint, a local server).
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dorkitude/webctl/internal/prompts"
)

// Defaults.
const (
	DefaultMaxTokens   = 500
	DefaultTimeout     = 60 * time.Second
	DefaultConcurrency = 4
	// NothingRelevant is what the prompt asks the model to reply when the
	// kept text has nothing for the goal; the caller drops such pages.
	NothingRelevant = "NOTHING RELEVANT"
)

// ModelEnv carries the configured model into a command backend, so one
// command line can serve several models ("claude -p --model
// \"$WEBCTL_SUMMARIZE_MODEL\""). It is also the variable that sets
// summarize.model, so an exported value reaches the command either way.
const ModelEnv = "WEBCTL_SUMMARIZE_MODEL"

// Backend names.
const (
	BackendCommand  = "command"
	BackendEndpoint = "endpoint"
)

// Config selects and parameterises a backend. Command wins when both are
// set. Zero value means summarizing is not configured.
type Config struct {
	// Command is a shell command line run with `sh -c`; the prompt arrives
	// on stdin and the summary is read from stdout.
	Command string
	// Endpoint is an OpenAI-compatible base URL (".../v1") or a full
	// /chat/completions URL.
	Endpoint string
	// Model is the model name sent to Endpoint, or handed to Command as
	// $WEBCTL_SUMMARIZE_MODEL; empty leaves a command to its own default.
	Model string
	// APIKey is sent as a Bearer token to Endpoint. Empty means none.
	APIKey string
	// MaxTokens caps the summary length at Endpoint (default 500).
	MaxTokens int
	// ReasoningEffort is passed to Endpoint when set ("none" turns off
	// thinking on models that reason by default). Dropped on a 400.
	ReasoningEffort string
	// Timeout bounds one page (default 60s).
	Timeout time.Duration
}

// Backend names the backend c selects: BackendCommand, BackendEndpoint, or
// "" when nothing is configured.
func (c Config) Backend() string {
	switch {
	case strings.TrimSpace(c.Command) != "":
		return BackendCommand
	case strings.TrimSpace(c.Endpoint) != "":
		return BackendEndpoint
	}
	return ""
}

// Configured reports whether any backend is set.
func (c Config) Configured() bool { return c.Backend() != "" }

// Input is one page to summarize.
type Input struct {
	Query, Goal string
	Title, URL  string
	// Chunks are the relevance-filtered pieces of the page, in page order.
	Chunks []string
}

// Usage is the endpoint's reported token counts; zero for commands.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Summarizer turns an Input into a short text.
type Summarizer interface {
	Summarize(ctx context.Context, in Input) (string, Usage, error)
	// Name describes the backend for logs ("command: claude -p ...").
	Name() string
}

// New builds a Summarizer from cfg, or returns an error when nothing is
// configured.
func New(cfg Config) (Summarizer, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	p, err := prompts.Load(prompts.Summarize)
	if err != nil {
		return nil, err
	}
	switch cfg.Backend() {
	case BackendCommand:
		return &commandSummarizer{cmd: strings.TrimSpace(cfg.Command), model: strings.TrimSpace(cfg.Model), timeout: cfg.Timeout, prompt: p}, nil
	case BackendEndpoint:
		if strings.TrimSpace(cfg.Model) == "" {
			return nil, errors.New("summarize: endpoint needs a model (summarize.model)")
		}
		return &endpointSummarizer{cfg: cfg, prompt: p, client: &http.Client{Timeout: cfg.Timeout}}, nil
	}
	return nil, errors.New("summarize: set summarize.command or summarize.endpoint (see `webctl docs summarize`)")
}

// render fills the prompt for one page.
func render(p *prompts.Prompt, in Input) (string, error) {
	return p.Render(prompts.Data{
		Query: in.Query, Goal: in.Goal, Title: in.Title, URL: in.URL,
		Chunk: strings.Join(in.Chunks, "\n\n"),
	})
}

// clean strips wrapping whitespace and code fences a model may add.
func clean(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// IsNothing reports whether a summary says the page had nothing relevant.
func IsNothing(summary string) bool {
	return strings.EqualFold(strings.Trim(strings.TrimSpace(summary), ".*_"), NothingRelevant)
}

// ---- command backend ----

type commandSummarizer struct {
	cmd     string
	model   string
	timeout time.Duration
	prompt  *prompts.Prompt
}

func (c *commandSummarizer) Name() string { return "command: " + c.cmd }

func (c *commandSummarizer) Summarize(ctx context.Context, in Input) (string, Usage, error) {
	prompt, err := render(c.prompt, in)
	if err != nil {
		return "", Usage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", c.cmd)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = os.Environ()
	if c.model != "" {
		// Appended after the inherited environment, so a --summarize-model
		// for this run wins over an exported value.
		cmd.Env = append(cmd.Env, ModelEnv+"="+c.model)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", Usage{}, fmt.Errorf("summarize command timed out after %s", c.timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", Usage{}, fmt.Errorf("summarize command failed: %s", msg)
	}
	out := clean(stdout.String())
	if out == "" {
		return "", Usage{}, errors.New("summarize command printed nothing")
	}
	return out, Usage{}, nil
}

// ---- OpenAI-compatible endpoint backend ----

type endpointSummarizer struct {
	cfg    Config
	prompt *prompts.Prompt
	client *http.Client
}

func (e *endpointSummarizer) Name() string {
	return "endpoint: " + e.cfg.Model + " at " + e.cfg.Endpoint
}

// url returns the chat completions URL for a base URL or a full one.
func (e *endpointSummarizer) url() string {
	u := strings.TrimRight(strings.TrimSpace(e.cfg.Endpoint), "/")
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return u + "/chat/completions"
}

func (e *endpointSummarizer) Summarize(ctx context.Context, in Input) (string, Usage, error) {
	prompt, err := render(e.prompt, in)
	if err != nil {
		return "", Usage{}, err
	}
	body := map[string]any{
		"model":       e.cfg.Model,
		"max_tokens":  e.cfg.MaxTokens,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	}
	if e.cfg.ReasoningEffort != "" {
		body["reasoning_effort"] = e.cfg.ReasoningEffort
	}
	text, usage, status, err := e.post(ctx, body)
	if status == http.StatusBadRequest && e.cfg.ReasoningEffort != "" {
		// Not every server knows reasoning_effort; try once without it.
		delete(body, "reasoning_effort")
		text, usage, _, err = e.post(ctx, body)
	}
	if err != nil {
		return "", usage, err
	}
	if text == "" {
		return "", usage, errors.New("summarize: endpoint returned an empty message (a reasoning model may have spent max_tokens thinking; set summarize.reasoning_effort to none or raise summarize.max_tokens)")
	}
	return text, usage, nil
}

func (e *endpointSummarizer) post(ctx context.Context, body map[string]any) (string, Usage, int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url(), bytes.NewReader(raw))
	if err != nil {
		return "", Usage{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", Usage{}, 0, fmt.Errorf("summarize: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", Usage{}, resp.StatusCode, fmt.Errorf("summarize: %s returned HTTP %d: %s", e.cfg.Model, resp.StatusCode, msg)
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", Usage{}, resp.StatusCode, fmt.Errorf("summarize: bad JSON from endpoint: %w", err)
	}
	usage := Usage{InputTokens: d.Usage.Prompt, OutputTokens: d.Usage.Completion}
	if len(d.Choices) == 0 {
		return "", usage, resp.StatusCode, errors.New("summarize: endpoint returned no choices")
	}
	return clean(d.Choices[0].Message.Content), usage, resp.StatusCode, nil
}
