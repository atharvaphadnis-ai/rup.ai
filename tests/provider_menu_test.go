package tests

// provider_menu_test.go — regression test for the /provider menu mapping
// bug fixed in v0.1.2. Symptom reported by a user: with deepseek,
// koboldcpp, ollama and qwen configured, the /provider menu displayed the
// unconfigured presets in one order but mapped the typed index over the
// full alphabetical preset list, so typing 8 (shown as Groq) opened
// Fireworks instead. The display order and the index→id mapping must come
// from the same source of truth (backend ProviderMenu).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderMenuMapping reproduces the reported session: four configured
// providers (deepseek, koboldcpp, ollama, qwen), then picks menu entry [8],
// which is displayed as Groq. The sign-in section that opens must be Groq.
func TestProviderMenuMapping(t *testing.T) {
	env := isolatedEnv(t)
	dir := t.TempDir()

	home := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			home = strings.TrimPrefix(kv, "HOME=")
		}
	}
	if home == "" {
		t.Fatal("isolated HOME not found in env")
	}
	cfgDir := filepath.Join(home, ".config", "rup.ai")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const cfgJSON = `{
 "version": 1,
 "active_provider": "koboldcpp",
 "active_model": "koboldcpp",
 "onboarded": true,
 "providers": {
  "deepseek":  {"kind": "openai", "auth": "api_key", "base_url": "https://api.deepseek.com/v1", "model": "deepseek-chat"},
  "koboldcpp": {"kind": "openai", "auth": "none", "base_url": "http://localhost:5001/v1", "model": "koboldcpp"},
  "ollama":    {"kind": "openai", "auth": "none", "base_url": "http://localhost:11434/v1", "model": "llama3.2"},
  "qwen":      {"kind": "openai", "auth": "api_key", "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1", "model": "qwen-plus"}
 },
 "policies": {}, "context": {}, "ui": {}, "web": {}
}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// drive the interactive session: open /provider, choose 8, then EOF
	out, stderr, code := runCLI(t, env, dir, "/provider\n8\n")
	if code != 0 && code != 1 {
		t.Fatalf("unexpected exit code %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "Sign in — groq") {
		t.Fatalf("selecting [8] must open the Groq sign-in flow; got:\n%s\nstderr:\n%s", out, stderr)
	}
	if strings.Contains(out, "Sign in — fireworks") {
		t.Fatalf("the jumbled mapping is back: [8] opened Fireworks:\n%s", out)
	}
	// the menu itself must list the four configured providers first, in
	// sorted order, and mark the active one
	if !strings.Contains(out, "koboldcpp  (configured, active)") {
		t.Fatalf("menu should mark the active provider; got:\n%s", out)
	}
}

// TestProviderMenuMappingSettings covers the same class of bug in the
// settings wizard ("Switch provider"): extra configured non-preset
// providers must map to themselves, not index into the preset list
// (which previously panicked on out-of-range selections).
func TestProviderMenuMappingSettings(t *testing.T) {
	env := isolatedEnv(t)
	dir := t.TempDir()

	home := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			home = strings.TrimPrefix(kv, "HOME=")
		}
	}
	if home == "" {
		t.Fatal("isolated HOME not found in env")
	}
	cfgDir := filepath.Join(home, ".config", "rup.ai")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// "my-gateway" is a configured provider that is NOT a preset — the old
	// settings code could not map it at all (index out of range).
	const cfgJSON = `{
 "version": 1,
 "active_provider": "my-gateway",
 "active_model": "gw-mini",
 "onboarded": true,
 "providers": {
  "my-gateway": {"kind": "openai", "auth": "api_key", "base_url": "https://gw.example.com/v1", "model": "gw-mini"}
 },
 "policies": {}, "context": {}, "ui": {}, "web": {}
}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// settings → "Switch provider" (option 1) → pick entry [2] = my-gateway
	// (the only configured provider, so it appears before the presets)
	out, stderr, code := runCLI(t, env, dir, "/settings\n1\n2\n")
	if code != 0 && code != 1 {
		t.Fatalf("unexpected exit code %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
	if !strings.Contains(out, "my-gateway  (configured, active)") {
		t.Fatalf("settings provider menu should list my-gateway as active; got:\n%s", out)
	}
}
