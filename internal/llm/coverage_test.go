package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/config"
	"github.com/madkoding/motita/internal/logx"
)

// --- New: every rejection path ----------------------------------------------

func TestNewRejectsUnsupportedProvider(t *testing.T) {
	if _, err := New(config.LLM{Provider: "telepathy", APIKey: "k"}, logx.Global()); err == nil {
		t.Error("an unsupported provider must be rejected")
	}
}

func TestNewRejectsMissingKey(t *testing.T) {
	if _, err := New(config.LLM{Provider: "openai"}, logx.Global()); err == nil {
		t.Error("a missing key must be rejected")
	}
}

// TestNewAppliesDefaults: a minimal configuration must end up complete, because
// the rest of the code assumes it.
func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(config.LLM{Provider: "openai", APIKey: "k"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Timeout <= 0 {
		t.Errorf("timeout = %v", c.cfg.Timeout)
	}
	if c.cfg.MaxAttempts < 1 {
		t.Errorf("max_attempts = %d", c.cfg.MaxAttempts)
	}
	if c.cfg.BackoffInitial <= 0 {
		t.Errorf("backoff_initial = %v", c.cfg.BackoffInitial)
	}
	if c.cfg.BackoffMax < c.cfg.BackoffInitial {
		t.Errorf("backoff_max (%v) must not be below backoff_initial (%v)", c.cfg.BackoffMax, c.cfg.BackoffInitial)
	}
	if c.log == nil {
		t.Error("it must use the global logger")
	}
	if c.sleep == nil {
		t.Error("sleep must be wired up")
	}
}

// TestNewNormalisesBackoffMax: a maximum below the initial value makes no sense
// and is corrected.
func TestNewNormalisesBackoffMax(t *testing.T) {
	c, err := New(config.LLM{
		Provider: "openai", APIKey: "k",
		BackoffInitial: 5 * time.Second, BackoffMax: time.Millisecond,
	}, logx.Global())
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.BackoffMax != c.cfg.BackoffInitial {
		t.Errorf("backoff_max = %v, expected %v", c.cfg.BackoffMax, c.cfg.BackoffInitial)
	}
}

// --- baseURL ----------------------------------------------------------------

func TestBaseURLDefaultsAndTrims(t *testing.T) {
	c, _ := New(config.LLM{Provider: "openai", APIKey: "k"}, logx.Global())

	if got := c.baseURL("https://default.example"); got != "https://default.example" {
		t.Errorf("with no base_url it must use the default: %q", got)
	}
	c.cfg.BaseURL = "https://custom.example/"
	if got := c.baseURL("https://default.example"); got != "https://custom.example" {
		t.Errorf("it must trim the trailing slash: %q", got)
	}
}

// --- OpenAI: error paths ----------------------------------------------------

func TestOpenAIEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("empty choices must be an error")
	}
}

// TestOpenAIEmptyContentIsRetryable: a 200 OK with a choice whose content is
// the empty string must be an error, not a silent success that propagates ""
// to ExtractJSON. This is the root cause of "the LLM response is empty".
func TestOpenAIEmptyContentIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("an empty content must be an error, not a silent success")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention the empty response, got: %v", err)
	}
}

// TestOpenAIEmptyContentToolsIsRetryable: same guard for the tools path —
// empty content AND no tool calls must be an error.
func TestOpenAIEmptyContentToolsIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"","tool_calls":null},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CompleteTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("empty content with no tool calls must be an error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention the empty response, got: %v", err)
	}
}

// TestOpenAIWhitespaceOnlyContentIsRetryable: whitespace-only content is just
// as useless as empty content and must be treated the same way.
func TestOpenAIWhitespaceOnlyContentIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"   \n  "},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("whitespace-only content must be an error")
	}
}

func TestOpenAIErrorInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("err = %v", err)
	}
}

func TestOpenAIUnreadableResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{this is not JSON`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("an unreadable response must be an error")
	}
}

// TestToOpenAIMessagesDefaultsRole: a message with no role becomes "user",
// because some providers reject an empty role.
func TestToOpenAIMessagesDefaultsRole(t *testing.T) {
	out := toOpenAIMessages([]Message{{Content: "no role"}, {Role: "assistant", Content: "a"}})
	if out[0].Role != "user" {
		t.Errorf("role = %q", out[0].Role)
	}
	if out[1].Role != "assistant" {
		t.Errorf("role = %q", out[1].Role)
	}
}

// --- Anthropic --------------------------------------------------------------

func TestAnthropicHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": "first "},
				{"type": "text", "text": "second"},
			},
		})
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "anthropic", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	got, err := c.Complete(context.Background(), []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "other", Content: "treated as user"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "first second" {
		t.Errorf("text = %q", got)
	}
}

func TestAnthropicNoText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[]}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "anthropic", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("a response with no text must be an error")
	}
}

func TestAnthropicUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json at all`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "anthropic", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("an unreadable response must be an error")
	}
}

// --- Gemini -----------------------------------------------------------------

func TestGeminiHappyPath(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []map[string]string{{"text": "gemini says hi"}}},
				"finishReason": "STOP",
			}},
		})
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "gemini", APIKey: "k", BaseURL: srv.URL, Model: "gemini-pro"}, logx.Global())
	got, err := c.Complete(context.Background(), []Message{
		{Role: "system", Content: "be brief"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "gemini says hi" {
		t.Errorf("text = %q", got)
	}
	// The system prompt must travel as systemInstruction, not as a message.
	if _, ok := received["systemInstruction"]; !ok {
		t.Errorf("systemInstruction is missing: %v", received)
	}
}

func TestGeminiNoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[]}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "gemini", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("no candidates must be an error")
	}
}

func TestGeminiEmptyText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "gemini", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Error("empty text must be an error")
	}
	if !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("the reason must be reported: %v", err)
	}
}

func TestGeminiUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "gemini", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("an unreadable response must be an error")
	}
}

// --- transport --------------------------------------------------------------

func TestPostInvalidURL(t *testing.T) {
	c, _ := New(config.LLM{Provider: "openai", APIKey: "k"}, logx.Global())
	if _, err := c.post(context.Background(), "://bad url", nil, map[string]any{}); err == nil {
		t.Error("an invalid URL must be an error")
	}
}

// TestPostUnserialisableBody covers the body serialisation failure: a channel
// cannot be marshalled to JSON.
func TestPostUnserialisableBody(t *testing.T) {
	c, _ := New(config.LLM{Provider: "openai", APIKey: "k"}, logx.Global())
	if _, err := c.post(context.Background(), "http://127.0.0.1:1", nil, make(chan int)); err == nil {
		t.Error("an unserialisable body must be an error")
	}
}

// --- helpers ----------------------------------------------------------------

func TestTruncateAndMaxInt(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate(strings.Repeat("x", 20), 5); got != "xxxxx..." {
		t.Errorf("truncate = %q", got)
	}
	if maxInt(3, 7) != 7 || maxInt(7, 3) != 7 {
		t.Error("maxInt is wrong")
	}
}

// TestDecodeJSONError: DecodeJSON must explain when the JSON does not fit.
func TestDecodeJSONError(t *testing.T) {
	var dest struct {
		Number int `json:"number"`
	}
	err := DecodeJSON(`{"number":"not a number"}`, &dest)
	if err == nil {
		t.Fatal("an incompatible JSON must be an error")
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("the error must explain the mismatch: %v", err)
	}

	if err := DecodeJSON("no json here", &dest); err == nil {
		t.Error("a response with no JSON must be an error")
	}
}

// TestRetryableHTTPError: a 4xx must not be retried, a 5xx must be.
func TestRetryableHTTPError(t *testing.T) {
	if retryable(&HTTPError{Code: 400, Body: "bad request"}) {
		t.Error("a 400 must not be retried")
	}
	if !retryable(&HTTPError{Code: 429, Body: "slow down"}) {
		t.Error("a 429 must be retried")
	}
	if !retryable(&HTTPError{Code: 500, Body: "boom"}) {
		t.Error("a 500 must be retried")
	}
	// Non-HTTP errors are treated as network errors (timeouts, DNS, dropped
	// connections) and are retried: that is the documented behaviour.
	if !retryable(errors.New("connection reset by peer")) {
		t.Error("a network error must be retried")
	}
}

// newTestClient builds a client against a test server, with no waiting between
// retries.
func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(config.LLM{
		Provider: "openai", APIKey: "k", BaseURL: baseURL,
		MaxAttempts: 1, Timeout: 5 * time.Second,
	}, logx.Global())
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(time.Duration) {}
	return c
}

// --- Remaining branches -----------------------------------------------------

// TestSleepIsWired: the default sleep must really wait (it is what separates the
// retries), and it must be replaceable so tests do not have to wait.
func TestSleepIsWired(t *testing.T) {
	c, err := New(config.LLM{Provider: "openai", APIKey: "k"}, logx.Global())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c.sleep(5 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 4*time.Millisecond {
		t.Errorf("sleep returned too early: %s", elapsed)
	}
}

// TestAnthropicErrorInBody: a 200 carrying an error object must not be taken as
// a valid answer.
func TestAnthropicErrorInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "anthropic", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("err = %v", err)
	}
}

// TestGeminiErrorInBody: the same for Gemini.
func TestGeminiErrorInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"quota exhausted"}}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "gemini", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("err = %v", err)
	}
}

// TestAnthropicAccumulatesSeveralSystemMessages: a second system message must be
// appended, not overwrite the first.
func TestAnthropicAccumulatesSystemMessages(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "anthropic", APIKey: "k", BaseURL: srv.URL}, logx.Global())
	if _, err := c.Complete(context.Background(), []Message{
		{Role: "system", Content: "first"},
		{Role: "system", Content: "second"},
		{Role: "user", Content: "hi"},
	}); err != nil {
		t.Fatal(err)
	}
	system, _ := received["system"].(string)
	if !strings.Contains(system, "first") || !strings.Contains(system, "second") {
		t.Errorf("system = %q", system)
	}
}

// TestPostUnreadableBody: a transport-level read failure must be reported.
func TestPostUnreadableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An announced length longer than what is sent makes the read fail.
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "short")
	}))
	defer srv.Close()

	c, _ := New(config.LLM{Provider: "openai", APIKey: "k", BaseURL: srv.URL, MaxAttempts: 1}, logx.Global())
	if _, err := c.post(context.Background(), srv.URL, nil, map[string]any{}); err == nil {
		t.Error("a truncated body must be an error")
	}
}

// TestAnthropicAndGeminiTransportError: when the transport fails, the error must
// travel up untouched from both dialects (not be swallowed as a parse failure).
func TestAnthropicAndGeminiTransportError(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			c, _ := New(config.LLM{
				Provider: provider, APIKey: "k",
				// A port that is closed: the connection fails immediately.
				BaseURL: "http://127.0.0.1:1", MaxAttempts: 1, Timeout: time.Second,
			}, logx.Global())
			c.sleep = func(time.Duration) {}
			if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err == nil {
				t.Error("a transport error must be reported")
			}
		})
	}
}
