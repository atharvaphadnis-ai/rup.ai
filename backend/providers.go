package main

// providers.go — the provider abstraction layer: shared message/tool types,
// the Provider interface implemented by the OpenAI-compatible and
// Anthropic-compatible clients, and the catalog of provider presets.

import (
        "context"
        "fmt"
        "sort"
)

// Role is a chat message role, normalized across providers.
type Role string

const (
        RoleSystem    Role = "system"
        RoleUser      Role = "user"
        RoleAssistant Role = "assistant"
        RoleTool      Role = "tool"
)

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
        ID        string `json:"id"`
        Name      string `json:"name"`
        Arguments string `json:"arguments"` // JSON object as string
}

// Message is a normalized conversation message.
type Message struct {
        Role       Role       `json:"role"`
        Content    string     `json:"content"`
        ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
        ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolDef is a provider-agnostic tool schema (JSON Schema parameters).
type ToolDef struct {
        Name        string         `json:"name"`
        Description string         `json:"description"`
        Parameters  map[string]any `json:"parameters"`
}

// Usage tracks token consumption as reported by the provider.
type Usage struct {
        InputTokens  int64 `json:"input_tokens"`
        OutputTokens int64 `json:"output_tokens"`
}

// Add accumulates usage.
func (u *Usage) Add(o Usage) {
        u.InputTokens += o.InputTokens
        u.OutputTokens += o.OutputTokens
}

// Total returns total tokens seen.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Caps describes provider feature support.
type Caps struct {
        Tools          bool
        Streaming      bool
        ModelDiscovery bool
}

// ChatRequest is a normalized request to any provider.
type ChatRequest struct {
        System      string
        Messages    []Message
        Tools       []ToolDef
        Model       string
        MaxTokens   int
        Temperature float64
        Stream      bool
}

// StreamEvent carries incremental output during streaming.
type StreamEvent struct {
        Delta string // text delta
        Usage Usage  // set on the final event when supported
        Done  bool
}

// StreamFunc receives streaming deltas; returning an error aborts the stream.
type StreamFunc func(StreamEvent) error

// StreamResult is the final assembled result of one model turn.
type StreamResult struct {
        Content   string
        ToolCalls []ToolCall
        Usage     Usage
}

// Provider is implemented by every supported AI backend.
type Provider interface {
        // ID returns the configured provider id (e.g. "openai", "zai", "ollama").
        ID() string
        // Kind returns the wire protocol: "openai" or "anthropic".
        Kind() string
        // Label is a human readable name.
        Label() string
        // Caps reports supported features.
        Caps() Caps
        // Stream performs one chat turn, streaming text deltas to on (may be nil).
        Stream(ctx context.Context, req ChatRequest, on StreamFunc) (StreamResult, error)
        // ListModels discovers models when the server supports it.
        ListModels(ctx context.Context) ([]string, error)
}

// NewProvider builds the right client for a provider config.
func NewProvider(id string, cfg *ProviderConfig, apiKey string) (Provider, error) {
        if cfg == nil {
                return nil, fmt.Errorf("provider %q is not configured", id)
        }
        if cfg.BaseURL == "" {
                return nil, fmt.Errorf("provider %q has no base URL; run `rup.ai setup`", id)
        }
        switch cfg.Kind {
        case "anthropic":
                return newAnthropicProvider(id, cfg, apiKey), nil
        case "openai", "":
                return newOpenAIProvider(id, cfg, apiKey), nil
        default:
                return nil, fmt.Errorf("unknown provider kind %q (supported: openai, anthropic)", cfg.Kind)
        }
}

// Preset is a known provider profile offered by the setup wizard.
type Preset struct {
        ID              string
        Label           string
        Kind            string // wire protocol
        BaseURL         string
        DefaultModel    string
        Models          []string
        ContextWindow   int
        MaxOutputTokens int
        AuthKind        string // "api_key" | "none"
        AuthNote        string // honest note about subscription vs API access
        DocsURL         string
        Local           bool
        Discovery       bool
}

// Presets is the catalog of officially documented endpoints.
//
// NOTE (accuracy): subscription (OAuth) access for OpenAI/ChatGPT and
// Anthropic/Claude is limited to those companies' first-party clients. For
// third-party tools like rup.ai these providers document API-key access only.
// See README "Authentication: API access vs subscription access".
var Presets = []Preset{
        {
                ID: "openai", Label: "OpenAI", Kind: "openai",
                BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-4o",
                Models:        []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "o3", "o4-mini"},
                ContextWindow: 128000, MaxOutputTokens: 16384, AuthKind: "api_key",
                AuthNote: "API key from platform.openai.com. ChatGPT Plus/Pro subscription sign-in is restricted to OpenAI's own clients; third-party apps must use the API.",
                DocsURL:  "https://platform.openai.com/docs", Discovery: true,
        },
        {
                ID: "anthropic", Label: "Anthropic", Kind: "anthropic",
                BaseURL: "https://api.anthropic.com", DefaultModel: "claude-sonnet-4-5",
                Models:        []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"},
                ContextWindow: 200000, MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from console.anthropic.com. Claude Pro/Max subscription login is exclusive to Anthropic's first-party apps (e.g. Claude Code); third-party tools must use the API.",
                DocsURL:  "https://docs.anthropic.com", Discovery: true,
        },
        {
                ID: "deepseek", Label: "DeepSeek", Kind: "openai",
                BaseURL: "https://api.deepseek.com/v1", DefaultModel: "deepseek-chat",
                Models: []string{"deepseek-chat", "deepseek-reasoner"}, ContextWindow: 128000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from platform.deepseek.com. No third-party subscription access is offered.",
                DocsURL:  "https://api-docs.deepseek.com", Discovery: true,
        },
        {
                ID: "qwen", Label: "Qwen (DashScope)", Kind: "openai",
                BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", DefaultModel: "qwen3-coder-plus",
                Models: []string{"qwen3-coder-plus", "qwen-plus", "qwen-max"}, ContextWindow: 128000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from Alibaba Cloud Model Studio (DashScope). Qwen Code's device login is for that CLI only; rup.ai uses the documented API.",
                DocsURL:  "https://www.alibabacloud.com/help/en/model-studio/", Discovery: true,
        },
        {
                ID: "kimi", Label: "Kimi (Moonshot AI)", Kind: "openai",
                BaseURL: "https://api.moonshot.ai/v1", DefaultModel: "kimi-k2-0905-preview",
                Models: []string{"kimi-k2-0905-preview", "kimi-k2-turbo-preview", "moonshot-v1-128k"}, ContextWindow: 128000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from platform.moonshot.ai (international) or platform.moonshot.cn (China). CN users may switch base_url to https://api.moonshot.cn/v1.",
                DocsURL:  "https://platform.moonshot.ai/docs", Discovery: true,
        },
        {
                ID: "zai", Label: "Z.ai (GLM)", Kind: "openai",
                BaseURL: "https://api.z.ai/api/paas/v4", DefaultModel: "glm-4.6",
                Models: []string{"glm-4.6", "glm-4.5", "glm-4.5-air"}, ContextWindow: 200000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from z.ai model console. Coding-plan subscriptions apply to Z.ai's own tooling; rup.ai uses the documented open platform API. CN users may use https://open.bigmodel.cn/api/paas/v4.",
                DocsURL:  "https://docs.z.ai", Discovery: true,
        },
        {
                ID: "minimax", Label: "MiniMax", Kind: "openai",
                BaseURL: "https://api.minimax.io/v1", DefaultModel: "MiniMax-M2",
                Models: []string{"MiniMax-M2", "MiniMax-Text-01"}, ContextWindow: 200000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from MiniMax open platform. Uses their OpenAI-compatible endpoint; if your region uses api.minimax.chat, edit the base URL in settings.",
                DocsURL:  "https://platform.minimax.io", Discovery: false,
        },
        {
                ID: "openrouter", Label: "OpenRouter", Kind: "openai",
                BaseURL: "https://openrouter.ai/api/v1", DefaultModel: "openrouter/auto",
                Models: []string{"openrouter/auto", "anthropic/claude-sonnet-4.5", "openai/gpt-4o", "google/gemini-2.0-flash-001"}, ContextWindow: 128000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from openrouter.ai — one key, many models behind it.",
                DocsURL:  "https://openrouter.ai/docs", Discovery: true,
        },
        {
                ID: "groq", Label: "Groq", Kind: "openai",
                BaseURL: "https://api.groq.com/openai/v1", DefaultModel: "llama-3.3-70b-versatile",
                Models: []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"}, ContextWindow: 128000,
                MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from console.groq.com.",
                DocsURL:  "https://console.groq.com/docs", Discovery: true,
        },
        {
                ID: "together", Label: "Together AI", Kind: "openai",
                BaseURL: "https://api.together.xyz/v1", DefaultModel: "Qwen/Qwen2.5-Coder-32B-Instruct",
                ContextWindow: 128000, MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from api.together.ai.",
                DocsURL:  "https://docs.together.ai", Discovery: true,
        },
        {
                ID: "fireworks", Label: "Fireworks AI", Kind: "openai",
                BaseURL: "https://api.fireworks.ai/inference/v1", DefaultModel: "accounts/fireworks/models/deepseek-v3",
                ContextWindow: 128000, MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from fireworks.ai.",
                DocsURL:  "https://docs.fireworks.ai", Discovery: true,
        },
        {
                ID: "siliconflow", Label: "SiliconFlow", Kind: "openai",
                BaseURL: "https://api.siliconflow.cn/v1", DefaultModel: "Qwen/Qwen2.5-Coder-32B-Instruct",
                ContextWindow: 128000, MaxOutputTokens: 8192, AuthKind: "api_key",
                AuthNote: "API key from siliconflow.cn.",
                DocsURL:  "https://docs.siliconflow.cn", Discovery: true,
        },
        {
                ID: "ollama", Label: "Ollama (local)", Kind: "openai",
                BaseURL: "http://127.0.0.1:11434/v1", DefaultModel: "qwen2.5-coder:7b",
                ContextWindow: 32768, MaxOutputTokens: 4096, AuthKind: "none", Local: true,
                AuthNote: "No key needed; rup.ai detects locally installed models via Ollama's native API.",
                DocsURL:  "https://ollama.com", Discovery: true,
        },
        {
                ID: "llamacpp", Label: "llama.cpp server (local)", Kind: "openai",
                BaseURL: "http://127.0.0.1:8080", DefaultModel: "local-gguf",
                ContextWindow: 32768, MaxOutputTokens: 4096, AuthKind: "none", Local: true,
                AuthNote: "Point at a running llama.cpp server (llama-server) exposing its OpenAI-compatible API.",
                DocsURL:  "https://github.com/ggml-org/llama.cpp", Discovery: true,
        },
        {
                ID: "koboldcpp", Label: "KoboldCpp (local)", Kind: "openai",
                BaseURL: "http://127.0.0.1:5001/v1", DefaultModel: "koboldcpp",
                ContextWindow: 32768, MaxOutputTokens: 4096, AuthKind: "none", Local: true,
                AuthNote: "Point at a running KoboldCpp instance; rup.ai reads its OpenAI-compatible endpoint.",
                DocsURL:  "https://github.com/LostRuins/koboldcpp", Discovery: false,
        },
        {
                ID: "custom", Label: "Custom OpenAI-compatible endpoint", Kind: "openai",
                BaseURL: "", DefaultModel: "",
                ContextWindow: 32768, MaxOutputTokens: 4096, AuthKind: "api_key",
                AuthNote: "Any OpenAI-compatible URL (vLLM, LM Studio, LiteLLM, gateways...).",
        },
}

// FindPreset looks a preset up by id.
func FindPreset(id string) (Preset, bool) {
        for _, p := range Presets {
                if p.ID == id {
                        return p, true
                }
        }
        return Preset{}, false
}

// PresetIDs lists preset ids.
func PresetIDs() []string {
        ids := make([]string, 0, len(Presets))
        for _, p := range Presets {
                ids = append(ids, p.ID)
        }
        sort.Strings(ids)
        return ids
}

// ProviderMenu builds the provider picker used by /provider and settings:
// configured providers first (sorted), then unconfigured presets (sorted).
// It returns the display options and the PARALLEL ids slice — the single
// source of truth that maps a 1-based selection index back to a provider
// id. Keeping display order and mapping in one place makes it impossible
// for the number you type to select a different provider than the line you
// saw (the v0.1.1 jumbled-menu bug).
func ProviderMenu(cfg *Config) ([]string, []string) {
        ids := cfg.ProviderIDs()
        presetIDs := PresetIDs()
        seen := make(map[string]bool, len(ids))
        opts := make([]string, 0, len(ids)+len(presetIDs))
        order := make([]string, 0, len(ids)+len(presetIDs))
        for _, id := range ids {
                label := id + "  (configured)"
                if id == cfg.ActiveProvider {
                        label = id + "  (configured, active)"
                }
                opts = append(opts, label)
                order = append(order, id)
                seen[id] = true
        }
        for _, id := range presetIDs {
                if seen[id] {
                        continue
                }
                p, _ := FindPreset(id)
                opts = append(opts, p.Label+"  ("+id+", not configured yet)")
                order = append(order, id)
        }
        return opts, order
}

// contextWindowOf returns a safe context window for the provider config.
func contextWindowOf(cfg *ProviderConfig) int {
        if cfg != nil && cfg.ContextWindow > 0 {
                return cfg.ContextWindow
        }
        return 32768
}

// maxOutputOf returns a safe max output token count.
func maxOutputOf(cfg *ProviderConfig) int {
        if cfg != nil && cfg.MaxOutputTokens > 0 {
                return cfg.MaxOutputTokens
        }
        return 4096
}
