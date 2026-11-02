package main

// anthropic_provider.go — client for the Anthropic Messages API
// (api.anthropic.com and compatible endpoints). Implements streaming (SSE),
// tool use, model discovery and proper header/auth handling. Anthropic
// requests are never sent to OpenAI-compatible endpoints and vice versa.

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

type anthropicProvider struct {
        id     string
        label  string
        cfg    *ProviderConfig
        apiKey string
        client *http.Client
}

func newAnthropicProvider(id string, cfg *ProviderConfig, apiKey string) *anthropicProvider {
        timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
        if timeout <= 0 {
                timeout = 5 * time.Minute
        }
        label := id
        if p, ok := FindPreset(id); ok {
                label = p.Label
        }
        return &anthropicProvider{
                id: id, label: label, cfg: cfg, apiKey: apiKey,
                client: &http.Client{Timeout: timeout},
        }
}

func (a *anthropicProvider) ID() string    { return a.id }
func (a *anthropicProvider) Kind() string  { return "anthropic" }
func (a *anthropicProvider) Label() string { return a.label }
func (a *anthropicProvider) Caps() Caps {
        return Caps{Tools: a.cfg.Tools, Streaming: a.cfg.Streaming, ModelDiscovery: true}
}

// --- wire formats ---------------------------------------------------------

type anthroContentBlock struct {
        Type  string          `json:"type"` // "text" | "tool_use" | "tool_result"
        Text  string          `json:"text,omitempty"`
        ID    string          `json:"id,omitempty"`
        Name  string          `json:"name,omitempty"`
        Input json.RawMessage `json:"input,omitempty"`
        // tool_result blocks:
        ToolUseID string          `json:"tool_use_id,omitempty"`
        Content   json.RawMessage `json:"content,omitempty"`
        IsError   bool            `json:"is_error,omitempty"`
}

type anthroMessage struct {
        Role    string          `json:"role"` // "user" | "assistant"
        Content json.RawMessage `json:"content"`
}

type anthroTool struct {
        Name        string         `json:"name"`
        Description string         `json:"description"`
        InputSchema map[string]any `json:"input_schema"`
}

type anthroRequest struct {
        Model       string          `json:"model"`
        System      string          `json:"system,omitempty"`
        Messages    []anthroMessage `json:"messages"`
        Tools       []anthroTool    `json:"tools,omitempty"`
        MaxTokens   int             `json:"max_tokens"`
        Temperature float64         `json:"temperature,omitempty"`
        Stream      bool            `json:"stream"`
}

type anthroResponse struct {
        Content []anthroContentBlock `json:"content"`
        Usage   struct {
                InputTokens  int64 `json:"input_tokens"`
                OutputTokens int64 `json:"output_tokens"`
        } `json:"usage"`
        Error *struct {
                Type    string `json:"type"`
                Message string `json:"message"`
        } `json:"error,omitempty"`
}

// --- conversion -----------------------------------------------------------

func toAnthroMessages(req ChatRequest) []anthroMessage {
        msgs := make([]anthroMessage, 0, len(req.Messages)+1)
        appendBlock := func(role string, b anthroContentBlock) {
                n := len(msgs)
                if n > 0 && msgs[n-1].Role == role {
                        var blocks []anthroContentBlock
                        if err := json.Unmarshal(msgs[n-1].Content, &blocks); err == nil {
                                blocks = append(blocks, b)
                                msgs[n-1].Content, _ = json.Marshal(blocks)
                                return
                        }
                }
                blocks := []anthroContentBlock{b}
                raw, _ := json.Marshal(blocks)
                msgs = append(msgs, anthroMessage{Role: role, Content: raw})
        }
        for _, m := range req.Messages {
                switch m.Role {
                case RoleUser:
                        appendBlock("user", anthroContentBlock{Type: "text", Text: m.Content})
                case RoleAssistant:
                        if m.Content != "" {
                                appendBlock("assistant", anthroContentBlock{Type: "text", Text: m.Content})
                        }
                        for _, tc := range m.ToolCalls {
                                appendBlock("assistant", anthroContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name,
                                        Input: json.RawMessage(safeJSONObj(tc.Arguments))})
                        }
                case RoleTool:
                        appendBlock("user", anthroContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID,
                                Content: json.RawMessage(mustJSONString(m.Content)), IsError: false})
                }
        }
        return msgs
}

func safeJSONObj(s string) string {
        s = strings.TrimSpace(s)
        if s == "" {
                return "{}"
        }
        if json.Valid([]byte(s)) {
                return s
        }
        // model produced invalid JSON arguments: wrap as string payload
        b, _ := json.Marshal(map[string]string{"raw": s})
        return string(b)
}

func mustJSONString(s string) string {
        b, err := json.Marshal(s)
        if err != nil {
                return `""`
        }
        return string(b)
}

// --- request --------------------------------------------------------------

// Stream performs one chat turn against the Anthropic Messages API.
func (a *anthropicProvider) Stream(ctx context.Context, req ChatRequest, on StreamFunc) (StreamResult, error) {
        reqWire := anthroRequest{
                Model:       req.Model,
                System:      req.System,
                Messages:    toAnthroMessages(req),
                MaxTokens:   req.MaxTokens,
                Temperature: req.Temperature,
                Stream:      req.Stream && a.cfg.Streaming,
        }
        if a.cfg.Tools && len(req.Tools) > 0 {
                reqWire.Tools = make([]anthroTool, 0, len(req.Tools))
                for _, t := range req.Tools {
                        reqWire.Tools = append(reqWire.Tools, anthroTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
                }
        }
        body, err := json.Marshal(reqWire)
        if err != nil {
                return StreamResult{}, err
        }

        var lastErr error
        for attempt := 0; attempt < 3; attempt++ {
                res, err := a.doOnce(ctx, body, reqWire.Stream, on)
                if err == nil {
                        return res, nil
                }
                lastErr = err
                if isContextErr(err) || !isRetryableStatus(err) {
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

func (a *anthropicProvider) doOnce(ctx context.Context, body []byte, stream bool, on StreamFunc) (StreamResult, error) {
        url := strings.TrimRight(a.cfg.BaseURL, "/") + "/v1/messages"
        httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
        if err != nil {
                return StreamResult{}, err
        }
        httpReq.Header.Set("Content-Type", "application/json")
        httpReq.Header.Set("anthropic-version", "2023-06-01")
        if a.apiKey != "" {
                // OAuth access tokens use Bearer; API keys use x-api-key.
                if strings.HasPrefix(a.apiKey, "sk-ant-oat") {
                        httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
                } else {
                        httpReq.Header.Set("x-api-key", a.apiKey)
                }
        }
        for k, v := range a.cfg.Headers {
                httpReq.Header.Set(k, v)
        }
        resp, err := a.client.Do(httpReq)
        if err != nil {
                return StreamResult{}, err
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
                raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
                var wire anthroResponse
                msg := strings.TrimSpace(string(raw))
                if json.Unmarshal(raw, &wire) == nil && wire.Error != nil {
                        return StreamResult{}, fmt.Errorf("status %d: %s: %s", resp.StatusCode, wire.Error.Type, wire.Error.Message)
                }
                return StreamResult{}, fmt.Errorf("status %d: %s", resp.StatusCode, truncateStr(msg, 500))
        }
        if stream {
                return a.readStream(resp, on)
        }
        return a.readFull(resp)
}

func (a *anthropicProvider) readFull(resp *http.Response) (StreamResult, error) {
        raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
        if err != nil {
                return StreamResult{}, err
        }
        var wire anthroResponse
        if err := json.Unmarshal(raw, &wire); err != nil {
                return StreamResult{}, fmt.Errorf("malformed response: %w", err)
        }
        if wire.Error != nil {
                return StreamResult{}, fmt.Errorf("%s: %s", wire.Error.Type, wire.Error.Message)
        }
        res := StreamResult{Usage: Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens}}
        for _, b := range wire.Content {
                switch b.Type {
                case "text":
                        res.Content += b.Text
                case "tool_use":
                        res.ToolCalls = append(res.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: string(b.Input)})
                }
        }
        return res, nil
}

// --- streaming ------------------------------------------------------------

type anthroStreamEvent struct {
        Type  string `json:"type"`
        Index int    `json:"index"`
        Delta struct {
                Type        string `json:"type"`
                Text        string `json:"text"`
                PartialJSON string `json:"partial_json"`
                StopReason  string `json:"stop_reason"`
        } `json:"delta"`
        ContentBlock struct {
                Type  string          `json:"type"`
                ID    string          `json:"id"`
                Name  string          `json:"name"`
                Input json.RawMessage `json:"input"`
        } `json:"content_block"`
        Message struct {
                Usage struct {
                        InputTokens int64 `json:"input_tokens"`
                } `json:"usage"`
        } `json:"message"`
        Usage struct {
                InputTokens  int64 `json:"input_tokens"`
                OutputTokens int64 `json:"output_tokens"`
        } `json:"usage"`
        Error *struct {
                Type    string `json:"type"`
                Message string `json:"message"`
        } `json:"error,omitempty"`
}

func (a *anthropicProvider) readStream(resp *http.Response, on StreamFunc) (StreamResult, error) {
        scanner := bufio.NewScanner(resp.Body)
        scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)

        var (
                res        StreamResult
                blocks     = map[int]*anthroContentBlock{}
                partialArgs = map[int]*strings.Builder{}
        )
        for scanner.Scan() {
                line := scanner.Text()
                if !strings.HasPrefix(line, "data:") {
                        continue
                }
                payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
                if payload == "" {
                        continue
                }
                var ev anthroStreamEvent
                if err := json.Unmarshal([]byte(payload), &ev); err != nil {
                        continue
                }
                switch ev.Type {
                case "error":
                        if ev.Error != nil {
                                return res, fmt.Errorf("%s: %s", ev.Error.Type, ev.Error.Message)
                        }
                case "message_start":
                        res.Usage.InputTokens = ev.Message.Usage.InputTokens
                case "content_block_start":
                        b := ev.ContentBlock
                        blocks[ev.Index] = &anthroContentBlock{Type: b.Type, ID: b.ID, Name: b.Name, Input: b.Input}
                        if b.Type == "tool_use" {
                                partialArgs[ev.Index] = &strings.Builder{}
                        }
                case "content_block_delta":
                        switch ev.Delta.Type {
                        case "text_delta":
                                if on != nil && ev.Delta.Text != "" {
                                        if err := on(StreamEvent{Delta: ev.Delta.Text}); err != nil {
                                                return res, err
                                        }
                                }
                                if blk, ok := blocks[ev.Index]; ok {
                                        blk.Text += ev.Delta.Text
                                }
                        case "input_json_delta":
                                if sb, ok := partialArgs[ev.Index]; ok {
                                        sb.WriteString(ev.Delta.PartialJSON)
                                }
                        }
                case "content_block_stop":
                        if blk, ok := blocks[ev.Index]; ok && blk.Type == "tool_use" {
                                args := "{}"
                                if sb, ok := partialArgs[ev.Index]; ok && sb.Len() > 0 {
                                        args = sb.String()
                                }
                                res.ToolCalls = append(res.ToolCalls, ToolCall{ID: blk.ID, Name: blk.Name, Arguments: safeJSONObj(args)})
                        }
                case "message_delta":
                        res.Usage.OutputTokens = ev.Usage.OutputTokens
                case "message_stop":
                        for _, blk := range blocks {
                                if blk.Type == "text" {
                                        res.Content += blk.Text
                                }
                        }
                        if on != nil {
                                _ = on(StreamEvent{Done: true, Usage: res.Usage})
                        }
                        return res, nil
                }
        }
        if err := scanner.Err(); err != nil {
                return res, fmt.Errorf("stream read: %w", err)
        }
        for _, blk := range blocks {
                if blk.Type == "text" {
                        res.Content += blk.Text
                }
        }
        return res, nil
}

// --- model discovery ------------------------------------------------------

// ListModels discovers models via GET /v1/models.
func (a *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
        url := strings.TrimRight(a.cfg.BaseURL, "/") + "/v1/models"
        httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
        if err != nil {
                return nil, err
        }
        httpReq.Header.Set("anthropic-version", "2023-06-01")
        if a.apiKey != "" {
                httpReq.Header.Set("x-api-key", a.apiKey)
        }
        for k, v := range a.cfg.Headers {
                httpReq.Header.Set(k, v)
        }
        resp, err := a.client.Do(httpReq)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
        var wire struct {
                Data []struct {
                        ID string `json:"id"`
                } `json:"data"`
                Error *struct {
                        Message string `json:"message"`
                } `json:"error"`
        }
        if err := json.Unmarshal(raw, &wire); err != nil {
                return nil, fmt.Errorf("model discovery not supported")
        }
        if wire.Error != nil {
                return nil, fmt.Errorf("%s", wire.Error.Message)
        }
        ids := make([]string, 0, len(wire.Data))
        for _, d := range wire.Data {
                ids = append(ids, d.ID)
        }
        return ids, nil
}
