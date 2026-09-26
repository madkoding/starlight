// Package llm implements Layer B: the reasoning engine.
//
// It is a lightweight, SDK-free client that talks to three families of API:
//
//   - openai:    POST /chat/completions  (Bearer)
//   - anthropic: POST /v1/messages       (x-api-key + anthropic-version)
//   - gemini:    POST /v1beta/models/<model>:generateContent (?key=)
//
// and claude-code, which drives the local claude CLI on the user's own Claude
// subscription instead of an HTTP API (see claudecode.go).
//
// Every provider is normalised to the same message structure and back to plain
// text, so the agent loop never needs to know which one is behind it. Parsing of
// structured responses and retrying with exponential backoff live here.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/madkoding/motita/internal/config"
	"github.com/madkoding/motita/internal/logx"
)

// Message is one turn of the conversation.
type Message struct {
	Role       string     // system | user | assistant | tool
	Content    string     // text of the turn (empty when the turn is only calls)
	ToolCalls  []ToolCall // for assistant turns that ask for tool results
	ToolCallID string     // for tool turns, matching the assistant call
}

// Client talks to the configured provider.
type Client struct {
	cfg   config.LLM
	http  *http.Client
	log   *logx.Logger
	sleep func(time.Duration) // injectable so tests do not have to wait
	// openStream opens one streaming attempt. It is injectable so the retry
	// wrapper can be tested against a producer that fails, or closes without a
	// done chunk, without having to make a real server misbehave.
	openStream func(context.Context, []Message, []Tool) (<-chan StreamChunk, error)
}

// New creates the reasoning engine client.
func New(cfg config.LLM, log *logx.Logger) (*Client, error) {
	if log == nil {
		log = logx.Global()
	}
	switch strings.ToLower(cfg.Provider) {
	case "openai", "ollama", "anthropic", "gemini", "codex", "copilot", "claude-code":
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %q", cfg.Provider)
	}
	// Ollama Cloud uses the OpenAI protocol; if no base URL is set we point at the
	// official endpoint so the user only has to provide the key.
	if strings.ToLower(cfg.Provider) == "ollama" && cfg.BaseURL == "" {
		cfg.BaseURL = "https://ollama.com/v1"
	}
	// Codex uses the same OpenAI-compatible protocol as openai, with the same
	// endpoint. The difference is only the model id (gpt-5-codex etc.).
	// Copilot also speaks the OpenAI chat completions protocol, but its base URL
	// is api.githubcopilot.com and its key is a short-lived Copilot token; the
	// caller is responsible for the token-exchange (see internal/llm/copilot.go).
	if cfg.APIKey == "" && config.ProviderNeedsKey(cfg.Provider) {
		return nil, errors.New("the LLM key is missing")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.BackoffInitial <= 0 {
		cfg.BackoffInitial = time.Second
	}
	if cfg.BackoffMax < cfg.BackoffInitial {
		cfg.BackoffMax = cfg.BackoffInitial
	}

	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				TLSClientConfig: TLSConfig(),
			},
		},
		log: log,
		sleep: func(d time.Duration) {
			time.Sleep(d)
		},
	}, nil
}

// openStreamOr returns the injected opener, or the real HTTP one.
func (c *Client) openStreamOr() func(context.Context, []Message, []Tool) (<-chan StreamChunk, error) {
	if c.openStream != nil {
		return c.openStream
	}
	switch strings.ToLower(c.cfg.Provider) {
	case "claude-code":
		return c.callClaudeCodeStream
	default:
		// openai, ollama, codex and copilot all speak /chat/completions.
		return c.callOpenAIToolsStream
	}
}

// Complete sends the conversation and returns the model's text, retrying with
// exponential backoff on transient failures.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	var last error
	wait := c.cfg.BackoffInitial

	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		text, err := c.call(ctx, messages)
		if err == nil {
			return text, nil
		}
		last = err

		if !retryable(err) {
			// A credentials or request error does not improve by retrying:
			// fail fast instead of burning time and quota.
			c.log.Error("LLM call failed with no possibility of retry",
				"attempt", attempt, "error", err)
			return "", err
		}
		if attempt == c.cfg.MaxAttempts {
			break
		}

		c.log.Warn("retrying LLM call",
			"attempt", attempt, "max_attempts", c.cfg.MaxAttempts,
			"wait", wait.String(), "error", err)

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("cancelled while waiting to retry: %w", ctx.Err())
		case <-time.After(wait):
		}
		wait *= 2
		if wait > c.cfg.BackoffMax {
			wait = c.cfg.BackoffMax
		}
	}

	return "", fmt.Errorf("all %d attempts were exhausted: %w", c.cfg.MaxAttempts, last)
}

// CompleteTools sends the conversation with the available tools and returns whatever
// the model answered: text, tool calls, or both. It uses the same retry policy as
// Complete so callers do not have to think about transient failures.
func (c *Client) CompleteTools(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	return c.completeWithTool(ctx, messages, tools)
}

// CompleteToolsStream is the streaming version of CompleteTools. It returns a
// channel that yields chunks as they arrive from the provider. The channel is always
// closed; the caller must read until StreamDone or StreamError.
func (c *Client) CompleteToolsStream(ctx context.Context, messages []Message, tools []Tool) <-chan StreamChunk {
	out := make(chan StreamChunk, 8)
	go func() {
		defer close(out)
		var last error
		wait := c.cfg.BackoffInitial
		for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
			chunkCh, err := c.openStreamOr()(ctx, messages, tools)
			if err == nil {
				forwarded := false
				for chunk := range chunkCh {
					// A failure before anything reached the caller is a failed attempt like
					// one that could not open: it goes through the same retry policy.
					if !forwarded && chunk.Event == StreamError {
						err = chunk.Error
						break
					}
					forwarded = true
					out <- chunk
					if chunk.Event == StreamError || chunk.Event == StreamDone {
						return
					}
				}
				if err == nil {
					return
				}
			}
			last = err
			if !retryable(err) {
				c.log.Error("LLM tool stream failed with no possibility of retry", "attempt", attempt, "error", err)
				out <- StreamChunk{Event: StreamError, Error: err}
				return
			}
			if attempt == c.cfg.MaxAttempts {
				break
			}
			c.log.Warn("retrying LLM tool stream", "attempt", attempt, "max_attempts", c.cfg.MaxAttempts, "wait", wait.String(), "error", err)
			select {
			case <-ctx.Done():
				out <- StreamChunk{Event: StreamError, Error: fmt.Errorf("cancelled while waiting to retry: %w", ctx.Err())}
				return
			case <-time.After(wait):
			}
			wait *= 2
			if wait > c.cfg.BackoffMax {
				wait = c.cfg.BackoffMax
			}
		}
		out <- StreamChunk{Event: StreamError, Error: fmt.Errorf("all %d attempts were exhausted: %w", c.cfg.MaxAttempts, last)}
	}()
	return out
}

func (c *Client) completeWithTool(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	var last error
	wait := c.cfg.BackoffInitial
	var empty Reply

	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		reply, err := c.callTools(ctx, messages, tools)
		if err == nil {
			return reply, nil
		}
		last = err

		if !retryable(err) {
			c.log.Error("LLM tool call failed with no possibility of retry",
				"attempt", attempt, "error", err)
			return empty, err
		}
		if attempt == c.cfg.MaxAttempts {
			break
		}

		c.log.Warn("retrying LLM tool call",
			"attempt", attempt, "max_attempts", c.cfg.MaxAttempts,
			"wait", wait.String(), "error", err)

		select {
		case <-ctx.Done():
			return empty, fmt.Errorf("cancelled while waiting to retry: %w", ctx.Err())
		case <-time.After(wait):
		}
		wait *= 2
		if wait > c.cfg.BackoffMax {
			wait = c.cfg.BackoffMax
		}
	}

	return empty, fmt.Errorf("all %d attempts were exhausted: %w", c.cfg.MaxAttempts, last)
}

// HTTPError describes a failure with a status code so that retryability can be
// decided.
type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Code, truncate(e.Body, 400))
}

// retryable says whether it is worth trying again.
func retryable(err error) bool {
	if errors.As(err, new(fatalError)) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Code == 429, he.Code >= 500:
			return true
		default:
			return false
		}
	}
	// Network errors (timeouts, DNS, dropped connection) are retried.
	return true
}

// call makes a single request to the provider.
func (c *Client) call(ctx context.Context, messages []Message) (string, error) {
	switch strings.ToLower(c.cfg.Provider) {
	case "anthropic":
		return c.callAnthropic(ctx, messages)
	case "gemini":
		return c.callGemini(ctx, messages)
	case "claude-code":
		return c.callClaudeCodeText(ctx, messages)
	default:
		// openai, ollama, codex and copilot all speak /chat/completions.
		return c.callOpenAI(ctx, messages)
	}
}

// callTools makes a single tool-enabled request to the provider.
func (c *Client) callTools(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	switch strings.ToLower(c.cfg.Provider) {
	case "anthropic":
		return c.callAnthropicTools(ctx, messages, tools)
	case "gemini":
		return c.callGeminiTools(ctx, messages, tools)
	case "claude-code":
		return c.callClaudeCode(ctx, messages, tools, nil)
	default:
		// openai, ollama, codex and copilot all speak /chat/completions.
		return c.callOpenAITools(ctx, messages, tools)
	}
}

func (c *Client) baseURL(defecto string) string {
	if c.cfg.BaseURL == "" {
		return defecto
	}
	return strings.TrimRight(c.cfg.BaseURL, "/")
}

// ListModels asks the provider for the catalogue it publishes and returns the
// model names in the order the host reports them.
//
// Two endpoint families are tried because the base URL may be either spelling:
//
//   - OpenAI-compatible: <base>/models  (works for OpenAI, Ollama Cloud with
//     base https://ollama.com/v1, Groq, OpenRouter, and similar hosts)
//   - Ollama native: <base without /v1>/api/tags
//
// The first one that answers with a usable list wins. Exported so the first-run
// wizard can present the live catalogue instead of a hard-coded one.
func ListModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("no API base URL to list models from")
	}

	// Build the candidate URLs in order. A base URL that ends in /v1 must not
	// produce /v1/api/tags: that is a 404 (measured against Ollama Cloud).
	candidates := []string{base + "/models"}
	if root, ok := strings.CutSuffix(base, "/v1"); ok {
		candidates = append(candidates, root+"/api/tags")
	} else {
		candidates = append(candidates, base+"/api/tags")
	}

	var firstErr error
	for _, url := range candidates {
		names, err := fetchModelList(ctx, url, apiKey)
		if err == nil && len(names) > 0 {
			return names, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("no models were listed by %s", base)
}

// fetchModelList performs one request and decodes both response shapes the two
// endpoint families use: {"data":[{"id":...}]} and {"models":[{"name":...}]}.
func fetchModelList(ctx context.Context, url, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: TLSConfig(),
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}

	var payload struct {
		// OpenAI-compatible spelling.
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		// Ollama native spelling.
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("could not decode the model list from %s: %w", url, err)
	}

	names := make([]string, 0, len(payload.Data)+len(payload.Models))
	seen := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, m := range payload.Data {
		add(m.ID)
	}
	for _, m := range payload.Models {
		add(m.Name)
	}
	return names, nil
}

// ListOllamaModels is kept as a named entry point for the Ollama provider; it is
// the same catalogue query.
func ListOllamaModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	return ListModels(ctx, baseURL, apiKey)
}

// --- OpenAI ----------------------------------------------------------------

type openAIMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) callOpenAI(ctx context.Context, messages []Message) (string, error) {
	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    toOpenAIMessages(messages),
		"max_tokens":  c.cfg.MaxTokens,
		"temperature": c.cfg.Temperature,
	}
	if c.cfg.Reasoning.Enabled && c.cfg.Reasoning.Level != "off" {
		body["reasoning_effort"] = c.cfg.Reasoning.Level
	}
	url := c.baseURL("https://api.openai.com/v1") + "/chat/completions"
	headers := map[string]string{"Authorization": "Bearer " + c.cfg.APIKey}

	data, err := c.post(ctx, url, headers, body)
	if err != nil {
		return "", err
	}
	var resp openAIResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("unreadable OpenAI response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", &HTTPError{Code: 400, Body: resp.Error.Message}
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("OpenAI returned empty choices")
	}
	choice := resp.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" {
		// An empty content with HTTP 200 is not a success: the model hit a
		// length limit, was blocked by a content filter, or answered with
		// tool_calls we cannot honour here. Treat it as a retryable error so
		// the Complete loop re-asks instead of propagating "" to ExtractJSON,
		// where it becomes the opaque "the LLM response is empty" failure.
		return "", fmt.Errorf("OpenAI returned an empty response (finish_reason=%q)", choice.FinishReason)
	}
	return choice.Message.Content, nil
}

func (c *Client) callOpenAITools(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    toOpenAIMessages(messages),
		"tools":       tools,
		"max_tokens":  c.cfg.MaxTokens,
		"temperature": c.cfg.Temperature,
	}
	if c.cfg.Reasoning.Enabled && c.cfg.Reasoning.Level != "off" {
		body["reasoning_effort"] = c.cfg.Reasoning.Level
	}
	url := c.baseURL("https://api.openai.com/v1") + "/chat/completions"
	headers := map[string]string{"Authorization": "Bearer " + c.cfg.APIKey}

	data, err := c.post(ctx, url, headers, body)
	if err != nil {
		return Reply{}, err
	}
	var resp openAIResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Reply{}, fmt.Errorf("unreadable OpenAI response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Reply{}, &HTTPError{Code: 400, Body: resp.Error.Message}
	}
	if len(resp.Choices) == 0 {
		return Reply{}, errors.New("OpenAI returned empty choices")
	}
	choice := resp.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" && len(choice.Message.ToolCalls) == 0 {
		// No text and no tool calls: the model produced nothing usable.
		// Treat it as a retryable error, same rationale as callOpenAI.
		return Reply{}, fmt.Errorf("OpenAI returned an empty response (finish_reason=%q)", choice.FinishReason)
	}
	return Reply{
		Content:      choice.Message.Content,
		Calls:        choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
	}, nil
}

// openAIStreamDelta is the incremental piece inside a streaming chunk.
type openAIStreamDelta struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls"`
}

// openAIStreamChunk is one SSE line from /chat/completions?stream=true.
type openAIStreamChunk struct {
	Choices []struct {
		Delta        openAIStreamDelta `json:"delta"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
}

func (c *Client) callOpenAIToolsStream(ctx context.Context, messages []Message, tools []Tool) (<-chan StreamChunk, error) {
	body := map[string]any{
		"model":          c.cfg.Model,
		"messages":       toOpenAIMessages(messages),
		"tools":          tools,
		"max_tokens":     c.cfg.MaxTokens,
		"temperature":    c.cfg.Temperature,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": false},
	}
	if c.cfg.Reasoning.Enabled && c.cfg.Reasoning.Level != "off" {
		body["reasoning_effort"] = c.cfg.Reasoning.Level
	}
	url := c.baseURL("https://api.openai.com/v1") + "/chat/completions"
	headers := map[string]string{"Authorization": "Bearer " + c.cfg.APIKey}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("could not serialise the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return nil, &HTTPError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	out := make(chan StreamChunk, 8)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		var acc StreamResult
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					out <- StreamChunk{Event: StreamDone, Reply: acc.FinalReply()}
				} else {
					out <- StreamChunk{Event: StreamError, Error: err}
				}
				return
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if bytes.HasPrefix(line, []byte(":")) {
				continue
			}
			const dataPrefix = "data: "
			if !bytes.HasPrefix(line, []byte(dataPrefix)) {
				continue
			}
			payload := bytes.TrimPrefix(line, []byte(dataPrefix))
			if string(payload) == "[DONE]" {
				out <- StreamChunk{Event: StreamDone, Reply: acc.FinalReply()}
				return
			}
			var chunk openAIStreamChunk
			if err := json.Unmarshal(payload, &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				out <- StreamChunk{Event: StreamText, Text: delta.Content}
			}
			for i := range delta.ToolCalls {
				out <- StreamChunk{Event: StreamToolCall, Call: &delta.ToolCalls[i]}
			}
			for _, tc := range delta.ToolCalls {
				acc.Handle(StreamChunk{Event: StreamToolCall, Call: &tc})
			}
			if delta.Content != "" {
				acc.Handle(StreamChunk{Event: StreamText, Text: delta.Content})
			}
			// finish_reason is by definition the end of the completion: the
			// provider sets it on the last chunk that carries the answer, and
			// [DONE] merely confirms it. Returning here — for any reason, not
			// only tool_calls — is what keeps a provider that omits [DONE] and
			// holds the connection open from hanging the caller until the
			// client timeout expires.
			if chunk.Choices[0].FinishReason != "" {
				out <- StreamChunk{Event: StreamDone, Reply: acc.FinalReply()}
				return
			}
		}
	}()
	return out, nil
}

func toOpenAIMessages(messages []Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(messages))
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		out = append(out, openAIMessage{
			Role:       role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

// --- Anthropic --------------------------------------------------------------

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		ID   string `json:"id"`
		Name string `json:"name"`
		// Input is the Anthropic function arguments object.
		Input map[string]any `json:"input"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) callAnthropic(ctx context.Context, messages []Message) (string, error) {
	// Anthropic receives the system prompt separately and does not accept the
	// "system" role inside the list.
	var system string
	conversation := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		conversation = append(conversation, map[string]any{
			"role": role,
			"content": []map[string]any{
				{"type": "text", "text": m.Content},
			},
		})
	}

	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    conversation,
		"max_tokens":  maxInt(c.cfg.MaxTokens, 1),
		"temperature": c.cfg.Temperature,
	}
	if system != "" {
		body["system"] = system
	}

	url := c.baseURL("https://api.anthropic.com") + "/v1/messages"
	headers := map[string]string{
		"x-api-key":         c.cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}

	data, err := c.post(ctx, url, headers, body)
	if err != nil {
		return "", err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("unreadable Anthropic response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", &HTTPError{Code: 400, Body: resp.Error.Message}
	}
	var sb strings.Builder
	for _, part := range resp.Content {
		if part.Type == "text" || part.Type == "" {
			sb.WriteString(part.Text)
		}
	}
	if sb.Len() == 0 {
		return "", errors.New("anthropic returned a response with no text")
	}
	return sb.String(), nil
}

func (c *Client) callAnthropicTools(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	var system string
	conversation := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		} else if m.Role == "tool" {
			role = "user"
		}
		var blocks []map[string]any
		if m.Content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": tc.Function.Arguments,
			})
		}
		if m.ToolCallID != "" {
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
		conversation = append(conversation, map[string]any{
			"role":    role,
			"content": blocks,
		})
	}

	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    conversation,
		"max_tokens":  maxInt(c.cfg.MaxTokens, 1),
		"temperature": c.cfg.Temperature,
		"tools":       toAnthropicTools(tools),
	}
	if system != "" {
		body["system"] = system
	}

	url := c.baseURL("https://api.anthropic.com") + "/v1/messages"
	headers := map[string]string{
		"x-api-key":         c.cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}

	data, err := c.post(ctx, url, headers, body)
	if err != nil {
		return Reply{}, err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Reply{}, fmt.Errorf("unreadable Anthropic response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Reply{}, &HTTPError{Code: 400, Body: resp.Error.Message}
	}

	reply := Reply{FinishReason: "stop"}
	for _, part := range resp.Content {
		switch part.Type {
		case "text":
			reply.Content += part.Text
		case "tool_use":
			args, _ := json.Marshal(part.Input)
			reply.Calls = append(reply.Calls, ToolCall{
				ID:   part.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      part.Name,
					Arguments: args,
				},
			})
		}
	}
	return reply, nil
}

func toAnthropicTools(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Function.Name,
			"description": t.Function.Description,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": t.Function.Parameters,
			},
		})
	}
	return out
}

// --- Gemini -----------------------------------------------------------------

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string          `json:"text"`
				FunctionCall json.RawMessage `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (c *Client) callGemini(ctx context.Context, messages []Message) (string, error) {
	var system string
	var contents []map[string]any
	for _, m := range messages {
		if m.Role == "system" {
			system += m.Content + "\n"
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": m.Content}},
		})
	}

	body := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": c.cfg.MaxTokens,
			"temperature":     c.cfg.Temperature,
		},
	}
	if system != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}

	base := c.baseURL("https://generativelanguage.googleapis.com")
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", base, c.cfg.Model, c.cfg.APIKey)

	data, err := c.post(ctx, url, nil, body)
	if err != nil {
		return "", err
	}
	var resp geminiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("unreadable Gemini response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", &HTTPError{Code: 400, Body: resp.Error.Message}
	}
	if len(resp.Candidates) == 0 {
		return "", errors.New("gemini returned a response with no candidates (safety block?)")
	}
	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		sb.WriteString(part.Text)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("gemini returned an empty response (finishReason=%q)", resp.Candidates[0].FinishReason)
	}
	return sb.String(), nil
}

func (c *Client) callGeminiTools(ctx context.Context, messages []Message, tools []Tool) (Reply, error) {
	var system string
	var contents []map[string]any
	for _, m := range messages {
		if m.Role == "system" {
			system += m.Content + "\n"
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		var parts []map[string]any
		if m.Content != "" {
			parts = append(parts, map[string]any{"text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			var args json.RawMessage
			if len(tc.Function.Arguments) > 0 {
				var obj map[string]any
				_ = json.Unmarshal(tc.Function.Arguments, &obj)
				args, _ = json.Marshal(obj)
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": tc.Function.Name,
					"args": args,
				},
			})
		}
		if m.ToolCallID != "" {
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{
					"name":     m.ToolCallID,
					"response": m.Content,
				},
			})
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"text": ""})
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": parts,
		})
	}

	body := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": c.cfg.MaxTokens,
			"temperature":     c.cfg.Temperature,
		},
		"tools": []map[string]any{
			{"functionDeclarations": toGeminiToolDeclarations(tools)},
		},
	}
	if system != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}

	base := c.baseURL("https://generativelanguage.googleapis.com")
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", base, c.cfg.Model, c.cfg.APIKey)

	data, err := c.post(ctx, url, nil, body)
	if err != nil {
		return Reply{}, err
	}
	var resp geminiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Reply{}, fmt.Errorf("unreadable Gemini response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return Reply{}, &HTTPError{Code: 400, Body: resp.Error.Message}
	}
	if len(resp.Candidates) == 0 {
		return Reply{}, errors.New("gemini returned a response with no candidates (safety block?)")
	}

	reply := Reply{FinishReason: resp.Candidates[0].FinishReason}
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			reply.Content += part.Text
		}
		if len(part.FunctionCall) > 0 {
			var fc geminiFunctionCall
			if err := json.Unmarshal(part.FunctionCall, &fc); err == nil {
				reply.Calls = append(reply.Calls, ToolCall{
					Type: "function",
					Function: FunctionCall{
						Name:      fc.Name,
						Arguments: fc.Args,
					},
				})
			}
		}
	}
	return reply, nil
}

func toGeminiToolDeclarations(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Function.Name,
			"description": t.Function.Description,
			"parameters": map[string]any{
				"type":       "object",
				"properties": t.Function.Parameters,
			},
		})
	}
	return out
}

// --- transport --------------------------------------------------------------

func (c *Client) post(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("could not serialise the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err // network error: retryable
	}
	defer resp.Body.Close()

	response, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Code: resp.StatusCode, Body: strings.TrimSpace(string(response))}
	}
	return response, nil
}

// truncate limits a text for error messages.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- Structured responses ---------------------------------------------------

var jsonBlock = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\}|\\[.*?\\])\\s*```")

// ExtractJSON gets the first JSON object out of an LLM response, tolerating the
// usual decorations: markdown blocks, text before and after, and nested braces.
func ExtractJSON(text string) ([]byte, error) {
	clean := strings.TrimSpace(text)
	if clean == "" {
		return nil, errors.New("the LLM response is empty")
	}

	if m := jsonBlock.FindStringSubmatch(clean); len(m) == 2 {
		return []byte(m[1]), nil
	}

	// First '{' and its balanced partner, respecting strings and escapes.
	start := strings.IndexAny(clean, "{[")
	if start < 0 {
		return nil, fmt.Errorf("the response contains no JSON: %q", truncate(clean, 200))
	}
	open := clean[start]
	closing := byte('}')
	if open == '[' {
		closing = ']'
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(clean); i++ {
		ch := clean[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// nothing
		case ch == open:
			depth++
		case ch == closing:
			depth--
			if depth == 0 {
				return []byte(clean[start : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("the JSON in the response is truncated: %q", truncate(clean, 200))
}

// DecodeJSON extracts and deserialises into dest, with explainable errors.
func DecodeJSON(text string, dest any) error {
	raw, err := ExtractJSON(text)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("the JSON in the response does not fit what was expected (%v): %s", err, truncate(string(raw), 300))
	}
	return nil
}
