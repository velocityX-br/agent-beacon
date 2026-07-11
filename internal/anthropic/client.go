// Package anthropic is a minimal Anthropic Messages API client built on the
// standard library (net/http) — no external SDK. It exists only to support the
// orchestrator's cross-model verifier, which needs a structured verdict via a
// forced tool_choice. The surface is intentionally small: one Messages call.
//
// The API key is never logged; RedactKey scrubs it from any error text before
// it surfaces to the user.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Messenger is the seam the orchestrator/verifier depend on so unit tests can
// stub the network. The real implementation is *Client.
type Messenger interface {
	Messages(ctx context.Context, req Request) (Response, error)
}

// Client is a tiny Messages API client. Construct with New.
type Client struct {
	key     string
	bearer  string // when set, auth via "Authorization: Bearer" instead of x-api-key
	baseURL string
	hc      *http.Client
}

// New returns a Client using the given API key and the public Anthropic base
// URL. The HTTP client carries a generous timeout suitable for verifier calls.
func New(key string) *Client {
	return &Client{
		key:     key,
		baseURL: defaultBaseURL,
		hc:      &http.Client{Timeout: 120 * time.Second},
	}
}

// WithBaseURL overrides the API base URL (useful for tests or proxies such as
// the Claude Code gateway pointed to by $ANTHROPIC_BASE_URL).
func (c *Client) WithBaseURL(u string) *Client {
	if u != "" {
		c.baseURL = strings.TrimRight(u, "/")
	}
	return c
}

// WithBearerToken switches auth to "Authorization: Bearer <token>" — the style
// used by $ANTHROPIC_AUTH_TOKEN when routing through a Claude Code proxy — in
// place of the direct-API x-api-key header.
func (c *Client) WithBearerToken(token string) *Client {
	if token != "" {
		c.bearer = token
	}
	return c
}

// Tool is a user-defined tool definition (JSON Schema input).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolChoice forces or hints tool selection. Type is "auto", "any", "tool", or
// "none"; Name is required when Type == "tool".
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Message is a single conversation turn. Content is plain text.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the subset of the Messages API request body we use.
type Request struct {
	Model      string      `json:"model"`
	MaxTokens  int         `json:"max_tokens"`
	System     string      `json:"system,omitempty"`
	Messages   []Message   `json:"messages"`
	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
}

// ContentBlock is one block of the assistant response. For a forced tool call,
// Type == "tool_use" and Input holds the structured arguments.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Response is the subset of the Messages API response we parse.
type Response struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []ContentBlock `json:"content"`
}

// ToolInput returns the raw JSON input of the first tool_use block whose name
// matches (or the first tool_use block if name is empty). ok is false when no
// matching tool_use block is present.
func (r Response) ToolInput(name string) (json.RawMessage, bool) {
	for _, b := range r.Content {
		if b.Type != "tool_use" {
			continue
		}
		if name == "" || b.Name == name {
			return b.Input, true
		}
	}
	return nil, false
}

// apiError mirrors the API's error envelope for readable messages.
type apiError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Messages performs POST /v1/messages. Network/decoding failures and non-2xx
// responses become errors with the API key redacted.
func (c *Client) Messages(ctx context.Context, req Request) (Response, error) {
	var out Response
	if c.key == "" && c.bearer == "" {
		return out, fmt.Errorf("anthropic: missing credentials")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return out, c.redact(fmt.Errorf("anthropic: build request: %w", err))
	}
	httpReq.Header.Set("content-type", "application/json")
	if c.bearer != "" {
		// Proxy/gateway auth (Claude Code $ANTHROPIC_AUTH_TOKEN style).
		httpReq.Header.Set("authorization", "Bearer "+c.bearer)
	} else {
		httpReq.Header.Set("x-api-key", c.key)
	}
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return out, c.redact(fmt.Errorf("anthropic: request failed: %w", err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, c.redact(fmt.Errorf("anthropic: read response: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae apiError
		if json.Unmarshal(data, &ae) == nil && ae.Error.Message != "" {
			return out, c.redact(fmt.Errorf("anthropic: %s (%d): %s", ae.Error.Type, resp.StatusCode, ae.Error.Message))
		}
		return out, c.redact(fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data))))
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, c.redact(fmt.Errorf("anthropic: decode response: %w", err))
	}
	return out, nil
}

// redact removes the credentials from an error's text so they never leak to logs.
func (c *Client) redact(err error) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	if c.key != "" {
		msg = strings.ReplaceAll(msg, c.key, "[REDACTED]")
	}
	if c.bearer != "" {
		msg = strings.ReplaceAll(msg, c.bearer, "[REDACTED]")
	}
	return fmt.Errorf("%s", msg)
}
