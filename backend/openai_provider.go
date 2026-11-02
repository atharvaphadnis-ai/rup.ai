package main

// openai_provider.go — client for every OpenAI-compatible chat API:
// OpenAI, DeepSeek, Qwen, Kimi, Z.ai/GLM, MiniMax, OpenRouter, Groq, Together,
// Fireworks, SiliconFlow, and local servers (Ollama, llama.cpp, KoboldCpp,
// vLLM, LM Studio). Implements streaming (SSE), tool calling, model discovery,
// timeouts, retries and per-provider extra headers.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type openAIProvider struct {
	id     string
	label  string
	cfg    *ProviderConfig
	apiKey string
	client *http.Client
}

func newOpenAIProvider(id string, cfg *ProviderConfig, apiKey string) *openAIProvider {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	label := id
	if p, ok := FindPreset(id); ok {
		label = p.Label
	}
	return &openAIProvider{
		id: id, label: label, cfg: cfg, apiKey: apiKey,
		client: &http.Client{Timeout: timeout},
	}
}

func (o *openAIProvider) ID() string     { return o.id }
func (o *openAIProvider) Kind() string   { return "openai" }
func (o *openAIProvider) Label() string  { return o.label }
func (o *openAIProvider) Caps() Caps {
	return Caps{Tools: o.cfg.Tools, Streaming: o.cfg.Streaming, ModelDiscovery: true}
}

// chatMessage is the wire format for one message.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatRequestWire struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
	StreamOpts  *streamOpts   `json:"stream_options,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolSchema `json:"function"`
}

type wireToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponseWire struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage   usageWire `json:"usage"`
	Error   *apiError `json:"error,omitempty"`
	Message string    `json:"message,omitempty"`
}

type usageWire struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *apiError) Error() string {
	code := ""
	if e.Code != nil {
		code = fmt.Sprintf(" (code %v)", e.Code)
	}
	return fmt.Sprintf("provider error: %s%s", e.Message, code)
}

// toWire converts normalized messages to the OpenAI wire format.
func toWire(req ChatRequest) []chatMessage {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: string(m.Role), Content: m.Content}
		switch m.Role {
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				idx := 0
				cm.ToolCalls = append(cm.ToolCalls, wireToolCall{Index: &idx, ID: tc.ID, Type: "function",
					Function: wireToolFunction{Name: tc.Name, Arguments: tc.Arguments}})
			}
		case RoleTool:
			cm.ToolCallID = m.ToolCallID
		}
		msgs = append(msgs, cm)
	}
	return msgs
}

// Stream performs one chat turn against an OpenAI-compatible endpoint.
func (o *openAIProvider) Stream(ctx context.Context, req ChatRequest, on StreamFunc) (StreamResult, error) {
	wire := chatRequestWire{
		Model:       req.Model,
		Messages:    toWire(req),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.Stream && o.cfg.Streaming {
		wire.Stream = true
		wire.StreamOpts = &streamOpts{IncludeUsage: true}
	}
	if o.cfg.Tools && len(req.Tools) > 0 {
		wire.Tools = make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			wire.Tools = append(wire.Tools, wireTool{Type: "function", Function: wireToolSchema{
				Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
		}
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return StreamResult{}, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		res, err := o.doOnce(ctx, body, req.Stream && o.cfg.Streaming, on)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var apiErr *apiError
		if isContextErr(err) {
			return StreamResult{}, err
		}
		if errorsAs(err, &apiErr) && !isRetryableStatus(err) {
			return StreamResult{}, err
		}
		select {
		case <-ctx.Done():
			return StreamResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 800 * time.Millisecond):
		}
	}
	return StreamResult{}, lastErr
}

func isContextErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// errorsAs is a tiny helper avoiding importing errors in several spots.
func errorsAs(err error, target **apiError) bool {
	if e, ok := err.(*apiError); ok {
		*target = e
		return true
	}
	return false
}

func isRetryableStatus(err error) bool {
	s := err.Error()
	return strings.Contains(s, "status 429") || strings.Contains(s, "status 500") ||
		strings.Contains(s, "status 502") || strings.Contains(s, "status 503") ||
		strings.Contains(s, "status 504") || strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection reset")
}

func (o *openAIProvider) doOnce(ctx context.Context, body []byte, stream bool, on StreamFunc) (StreamResult, error) {
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return StreamResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	for k, v := range o.cfg.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return StreamResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var wire chatResponseWire
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &wire) == nil && wire.Error != nil {
			return StreamResult{}, fmt.Errorf("status %d: %w", resp.StatusCode, wire.Error)
		}
		return StreamResult{}, fmt.Errorf("status %d: %s", resp.StatusCode, truncateStr(msg, 500))
	}

	if stream {
		return o.readStream(resp, on)
	}
	return o.readFull(resp)
}

func (o *openAIProvider) readFull(resp *http.Response) (StreamResult, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return StreamResult{}, err
	}
	var wire chatResponseWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return StreamResult{}, fmt.Errorf("malformed response: %w", err)
	}
	if wire.Error != nil {
		return StreamResult{}, wire.Error
	}
	res := StreamResult{Usage: Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens}}
	if len(wire.Choices) > 0 {
		m := wire.Choices[0].Message
		res.Content = m.Content
		for _, tc := range m.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	return res, nil
}

// sseChunk is one SSE data payload of the streaming format.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
	Error *apiError  `json:"error,omitempty"`
}

func (o *openAIProvider) readStream(resp *http.Response, on StreamFunc) (StreamResult, error) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)
	var (
		res      StreamResult
		tcByID   = map[string]*ToolCall{}
		tcOrder  []string
	)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alives / malformed fragments
		}
		if chunk.Error != nil {
			return res, chunk.Error
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				res.Content += c.Delta.Content
				if on != nil {
					if err := on(StreamEvent{Delta: c.Delta.Content}); err != nil {
						return res, err
					}
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				id := tc.ID
				if id == "" && tc.Index != nil {
					id = fmt.Sprintf("idx-%d", *tc.Index)
				}
				cur, ok := tcByID[id]
				if !ok {
					cur = &ToolCall{ID: firstNonEmpty(tc.ID, id)}
					tcByID[id] = cur
					tcOrder = append(tcOrder, id)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					cur.Arguments += tc.Function.Arguments
				}
			}
		}
		if chunk.Usage != nil {
			res.Usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("stream read: %w", err)
	}
	for _, id := range tcOrder {
		res.ToolCalls = append(res.ToolCalls, *tcByID[id])
	}
	if on != nil {
		_ = on(StreamEvent{Done: true, Usage: res.Usage})
	}
	return res, nil
}

// modelsWire is the /models discovery response.
type modelsWire struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

// ListModels discovers available models via GET /models.
func (o *openAIProvider) ListModels(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(o.cfg.BaseURL, "/") + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	for k, v := range o.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var wire modelsWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("model discovery not supported (%s)", truncateStr(strings.TrimSpace(string(raw)), 120))
	}
	if wire.Error != nil {
		return nil, wire.Error
	}
	ids := make([]string, 0, len(wire.Data))
	for _, d := range wire.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
