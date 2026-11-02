package main

// config.go — configuration persistence, profiles, environment overrides and
// settings model for rup.ai. Settings live in the OS config directory
// (e.g. %APPDATA%/rup.ai on Windows, ~/.config/rup.ai on Linux/macOS).
// Credentials are NOT stored here; they live in the OS credential store
// (see auth.go) or, as a fallback, in a 0600-permission credentials file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const configVersion = 1

// ProviderConfig describes one configured AI provider endpoint.
type ProviderConfig struct {
	Kind            string            `json:"kind"` // "openai" | "anthropic"
	BaseURL         string            `json:"base_url"`
	Model           string            `json:"model"`
	ContextWindow   int               `json:"context_window"`
	MaxOutputTokens int               `json:"max_output_tokens"`
	Temperature     float64           `json:"temperature"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	Streaming       bool              `json:"streaming"`
	Tools           bool              `json:"tools"`
	Headers         map[string]string `json:"headers,omitempty"`
	Auth            string            `json:"auth"` // "api_key" | "oauth" | "none"
	OAuth           *OAuthConfig      `json:"oauth,omitempty"`
}

// OAuthConfig carries optional OAuth endpoints for providers/gateways that
// publish an official third-party OAuth flow.
type OAuthConfig struct {
	Kind     string   `json:"kind"` // "device" | "code_pkce"
	AuthURL  string   `json:"auth_url"`
	TokenURL string   `json:"token_url"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes,omitempty"`
}

// Policies control agent autonomy and command execution security.
type Policies struct {
	Approval              string `json:"approval"` // "restricted" | "standard" | "permissive"
	WorkspaceJail         bool   `json:"workspace_jail"`
	CommandTimeoutSeconds int    `json:"command_timeout_seconds"`
	MaxAgentSteps         int    `json:"max_agent_steps"`
	AgentTimeoutMinutes   int    `json:"agent_timeout_minutes"`
}

// ContextCfg bounds how much project material is assembled per request.
type ContextCfg struct {
	BudgetTokens int `json:"budget_tokens"`
	TreeMaxLines int `json:"tree_max_lines"`
	MaxFileLines int `json:"max_file_lines"`
}

// UICfg controls terminal presentation.
type UICfg struct {
	Theme     string `json:"theme"`
	Animation bool   `json:"animation"`
	Plain     bool   `json:"plain"`
}

// WebCfg controls the optional localhost companion web UI.
type WebCfg struct {
	Enabled bool   `json:"enabled"`
	Bind    string `json:"bind"`
	Port    int    `json:"port"`
}

// Profile is a named provider/model preset for quick switching.
type Profile struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Config is the persisted settings document.
type Config struct {
	Version        int                        `json:"version"`
	ActiveProvider string                     `json:"active_provider"`
	ActiveModel    string                     `json:"active_model"`
	Providers      map[string]*ProviderConfig `json:"providers"`
	Policies       Policies                   `json:"policies"`
	Context        ContextCfg                 `json:"context"`
	UI             UICfg                      `json:"ui"`
	Web            WebCfg                     `json:"web"`
	Profiles       map[string]Profile         `json:"profiles,omitempty"`
	ActiveProfile  string                     `json:"active_profile,omitempty"`
	Onboarded      bool                       `json:"onboarded"`
}

// ConfigDir returns the directory holding rup.ai settings.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve config directory: %w", err)
	}
	return filepath.Join(base, "rup.ai"), nil
}

// DataDir returns the directory for backups, audit log and downloads.
func DataDir() (string, error) {
	base := os.Getenv("RUPAI_DATA_DIR")
	if base != "" {
		return base, nil
	}
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "rup.ai"), nil
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "rup.ai"), nil
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "rup.ai"), nil
	}
}

// ConfigPath is the settings file location.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// DefaultConfig returns sane defaults for a fresh installation.
func DefaultConfig() *Config {
	return &Config{
		Version:   configVersion,
		Providers: map[string]*ProviderConfig{},
		Policies: Policies{
			Approval:              "standard",
			WorkspaceJail:         true,
			CommandTimeoutSeconds: 180,
			MaxAgentSteps:         30,
			AgentTimeoutMinutes:   15,
		},
		Context: ContextCfg{BudgetTokens: 24000, TreeMaxLines: 120, MaxFileLines: 1600},
		UI:      UICfg{Theme: "dark", Animation: true, Plain: false},
		Web:     WebCfg{Enabled: true, Bind: "127.0.0.1", Port: 8618},
	}
}

// LoadConfig reads config.json, applies environment overrides and returns it.
// A missing file yields DefaultConfig without error.
func LoadConfig() (*Config, error) {
	cfg := DefaultConfig()
	path, err := ConfigPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return cfg, fmt.Errorf("config %s is invalid: %w", path, err)
		}
		if cfg.Providers == nil {
			cfg.Providers = map[string]*ProviderConfig{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	if cfg.ActiveProfile != "" {
		if p, ok := cfg.Profiles[cfg.ActiveProfile]; ok {
			if p.Provider != "" {
				cfg.ActiveProvider = p.Provider
			}
			if p.Model != "" {
				cfg.ActiveModel = p.Model
			}
		}
	}
	applyEnvOverrides(cfg)
	return cfg, nil
}

// applyEnvOverrides lets tests and power users configure without editing files.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("RUPAI_PROVIDER"); v != "" {
		cfg.ActiveProvider = strings.ToLower(v)
	}
	if v := os.Getenv("RUPAI_MODEL"); v != "" {
		cfg.ActiveModel = v
	}
	if v := os.Getenv("RUPAI_BASE_URL"); v != "" && cfg.ActiveProvider != "" {
		p := cfg.EnsureProvider(cfg.ActiveProvider, "openai")
		p.BaseURL = v
	}
	if v := os.Getenv("RUPAI_KIND"); v != "" && cfg.ActiveProvider != "" {
		cfg.EnsureProvider(cfg.ActiveProvider, v).Kind = v
	}
	if v := os.Getenv("RUPAI_APPROVAL"); v != "" {
		cfg.Policies.Approval = v
	}
	if v := os.Getenv("RUPAI_CONTEXT_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Context.BudgetTokens = n
		}
	}
	if v := os.Getenv("RUPAI_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Policies.MaxAgentSteps = n
		}
	}
	if _, ok := os.LookupEnv("RUPAI_NO_ANIM"); ok {
		cfg.UI.Animation = false
	}
	if _, ok := os.LookupEnv("RUPAI_PLAIN"); ok {
		cfg.UI.Plain = true
	}
}

// EnsureProvider returns the provider entry, creating a default one if absent.
func (c *Config) EnsureProvider(id, kind string) *ProviderConfig {
	if p, ok := c.Providers[id]; ok && p != nil {
		return p
	}
	preset, known := FindPreset(id)
	p := &ProviderConfig{
		Kind:            kind,
		BaseURL:         preset.BaseURL,
		Model:           preset.DefaultModel,
		ContextWindow:   128000,
		MaxOutputTokens: 8192,
		Temperature:     0.2,
		TimeoutSeconds:  300,
		Streaming:       true,
		Tools:           true,
		Auth:            preset.AuthKind,
	}
	if known {
		p.Kind = preset.Kind
		if kind != "" {
			p.Kind = kind
		}
		p.ContextWindow = preset.ContextWindow
		p.MaxOutputTokens = preset.MaxOutputTokens
		if preset.MaxOutputTokens > 0 {
			p.MaxOutputTokens = preset.MaxOutputTokens
		}
		if preset.AuthKind != "" {
			p.Auth = preset.AuthKind
		}
		if p.Auth == "" {
			p.Auth = "api_key"
		}
	}
	if p.Auth == "" {
		p.Auth = "api_key"
	}
	if kind != "" {
		p.Kind = kind
	}
	c.Providers[id] = p
	return p
}

// Active returns the active provider config, if any.
func (c *Config) Active() (*ProviderConfig, bool) {
	if c.ActiveProvider == "" {
		return nil, false
	}
	p, ok := c.Providers[c.ActiveProvider]
	if !ok || p == nil {
		return nil, false
	}
	return p, true
}

// Configured reports whether at least one usable provider is set up.
func (c *Config) Configured() bool {
	p, ok := c.Active()
	return ok && p.BaseURL != "" && (c.ActiveModel != "" || p.Model != "")
}

// Save persists the configuration with restrictive permissions.
func (c *Config) Save() error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ProviderIDs lists configured provider ids, sorted.
func (c *Config) ProviderIDs() []string {
	ids := make([]string, 0, len(c.Providers))
	for id := range c.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Redacted returns a deep copy safe to print or export: secrets removed.
func (c *Config) Redacted() Config {
	cp := *c
	cp.Providers = map[string]*ProviderConfig{}
	for id, p := range c.Providers {
		pp := *p
		pp.Headers = map[string]string{}
		for k, v := range p.Headers {
			if isSensitiveHeader(k) {
				pp.Headers[k] = "***redacted***"
			} else {
				pp.Headers[k] = v
			}
		}
		if pp.OAuth != nil {
			o := *pp.OAuth
			pp.OAuth = &o // ClientID is not secret; tokens live in the secret store
		}
		cp.Providers[id] = &pp
	}
	return cp
}

func isSensitiveHeader(k string) bool {
	kk := strings.ToLower(k)
	return strings.Contains(kk, "key") || strings.Contains(kk, "token") || strings.Contains(kk, "authorization") || strings.Contains(kk, "secret")
}

// Profile management -------------------------------------------------------

// UseProfile activates a stored profile by name.
func (c *Config) UseProfile(name string) error {
	p, ok := c.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q does not exist", name)
	}
	c.ActiveProfile = name
	if p.Provider != "" {
		c.ActiveProvider = p.Provider
	}
	if p.Model != "" {
		c.ActiveModel = p.Model
	}
	return c.Save()
}

// SaveProfile creates or updates a profile snapshot of the current selection.
func (c *Config) SaveProfile(name string) error {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = Profile{Provider: c.ActiveProvider, Model: c.ActiveModel}
	return c.Save()
}

// ExportNonSensitive renders settings without any credential material.
func ExportNonSensitive(c *Config) ([]byte, error) {
	return json.MarshalIndent(c.Redacted(), "", "  ")
}
