package main

// setup.go — the first-launch interactive setup wizard, the interactive
// settings interface (`rup.ai config` / /settings), the welcome screen and
// the `doctor` environment check. Users never edit config files by hand.

import (
        "context"
        "fmt"
        "os"
        "os/exec"
        "path/filepath"
        "sort"
        "strconv"
        "strings"
        "time"
)

// NeedsSetup reports whether the first-launch wizard should run.
func NeedsSetup(cfg *Config) bool {
        if cfg.Onboarded && cfg.Configured() {
                return false
        }
        return !cfg.Configured()
}

// RunWizard is the first-launch setup flow. Every choice is interactive;
// nothing is saved until the user confirms.
func RunWizard(cfg *Config, tui *Terminal) error {
        tui.Section("Welcome to rup.ai")
        tui.Block("About", "rup.ai is your AI-powered software development agent.\n"+
                "It plans, writes, runs and fixes real code — right in your terminal.\n"+
                "Let's configure your development environment (you can change everything later).")

        tui.Block("Choose your AI connection", "")
        options := []string{
                "OpenAI-compatible API  (OpenAI, DeepSeek, Qwen, Kimi, Z.ai/GLM, MiniMax, OpenRouter, Groq, Together, Fireworks, SiliconFlow, custom)",
                "Anthropic-compatible API (Claude)",
                "Sign in with your provider account (officially supported methods)",
                "Connect to Ollama (local)",
                "Connect to llama.cpp server (local)",
                "Connect to KoboldCpp (local)",
                "Load a local GGUF model",
                "Configure a custom provider",
                "Configure later (skip)",
        }
        sel := tui.Choice("Provider:", options, 1)

        switch sel {
        case 1, 2, 8:
                return wizardCloudProvider(cfg, tui, sel)
        case 3:
                return wizardLogin(cfg, tui)
        case 4, 5, 6:
                return wizardLocalProvider(cfg, tui, sel)
        case 7:
                return wizardGGUF(cfg, tui)
        default:
                tui.Info("Setup skipped.")
                tui.Block("Configure later", "Run `rup.ai setup` any time, or set a provider with:\n"+
                        "  rup.ai config        interactive settings\n"+
                        "  rup.ai login <id>    store an API key\n"+
                        "Environment variables also work (see README: RUPAI_PROVIDER, RUPAI_API_KEY, RUPAI_BASE_URL, RUPAI_MODEL).")
                cfg.Onboarded = false
                return cfg.Save()
        }
}

// wizardCloudProvider walks through an OpenAI/Anthropic-compatible provider.
func wizardCloudProvider(cfg *Config, tui *Terminal, sel int) error {
        var presetIDs []string
        for _, p := range Presets {
                if sel == 2 && p.Kind != "anthropic" {
                        continue
                }
                if sel == 8 && p.ID != "custom" {
                        continue
                }
                if sel == 1 && (p.Kind == "anthropic" || p.Local || p.ID == "custom") {
                        continue
                }
                presetIDs = append(presetIDs, p.ID)
        }
        sort.Strings(presetIDs)
        opts := make([]string, 0, len(presetIDs)+1)
        for _, id := range presetIDs {
                p, _ := FindPreset(id)
                opts = append(opts, p.Label+"  ("+id+")")
        }
        opts = append(opts, "Other — type an id")
        idx := tui.Choice("Which provider?", opts, 1)

        providerID := ""
        baseURL := ""
        model := ""
        kind := "openai"
        if idx <= len(presetIDs) {
                id := presetIDs[idx-1]
                p, _ := FindPreset(id)
                providerID = id
                baseURL = p.BaseURL
                model = p.DefaultModel
                kind = p.Kind
                if p.AuthNote != "" {
                        tui.Block("About " + p.Label + " access", p.AuthNote)
                }
                if id == "custom" {
                        providerID = strings.ToLower(strings.TrimSpace(mustReadLine(tui, "Provider id (e.g. my-gateway): ")))
                        if providerID == "" {
                                return fmt.Errorf("setup cancelled")
                        }
                        baseURL = strings.TrimSpace(mustReadLine(tui, "Base URL (e.g. https://api.example.com/v1): "))
                        model = strings.TrimSpace(mustReadLine(tui, "Default model id: "))
                        if strings.Contains(baseURL, "anthropic") {
                                kind = "anthropic"
                        }
                }
        } else {
                providerID = strings.ToLower(strings.TrimSpace(mustReadLine(tui, "Provider id: ")))
                if providerID == "" {
                        return fmt.Errorf("setup cancelled")
                }
                baseURL = strings.TrimSpace(mustReadLine(tui, "Base URL: "))
                model = strings.TrimSpace(mustReadLine(tui, "Model id: "))
        }
        if baseURL == "" {
                return fmt.Errorf("a base URL is required")
        }

        pcfg := cfg.EnsureProvider(providerID, kind)
        pcfg.BaseURL = baseURL
        if model != "" {
                pcfg.Model = model
        }
        if cfg.ActiveModel == "" {
                cfg.ActiveModel = pcfg.Model
        }

        // authentication
        if kind == "anthropic" {
                pcfg.Kind = "anthropic"
        }
        if err := loginIfNeeded(cfg, tui, providerID, pcfg); err != nil {
                return err
        }

        // model selection (discovery when supported)
        prov, err := buildProvider(cfg, providerID)
        if err == nil && prov.Caps().ModelDiscovery {
                stop := tui.Spinner("fetching model list")
                ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
                models, err := prov.ListModels(ctx)
                cancel()
                stop()
                if err == nil && len(models) > 0 {
                        // show a bounded list
                        shown := models
                        if len(shown) > 40 {
                                shown = shown[:40]
                        }
                        opts := append(shown, "Type a model id manually")
                        mi := tui.Choice("Pick a model:", opts, 1)
                        if mi <= len(shown) {
                                cfg.ActiveModel = shown[mi-1]
                        } else {
                                cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model id: "))
                        }
                }
                if err != nil {
                        tui.Info("model discovery unavailable (" + err.Error() + "); using " + cfg.ActiveModel)
                }
        }
        if cfg.ActiveModel == "" {
                cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model id: "))
        }

        // test connection before saving
        tui.Info("testing connection…")
        if err := testAndReport(cfg, providerID); err != nil {
                if !tui.Confirm("Connection test failed — save anyway?", false) {
                        return err
                }
        }
        return finishSetup(cfg, tui, providerID)
}

// wizardLogin handles choice [3]: account sign-in with honest guidance.
func wizardLogin(cfg *Config, tui *Terminal) error {
        ids := PresetIDs()
        opts := make([]string, 0, len(ids))
        for _, id := range ids {
                p, _ := FindPreset(id)
                opts = append(opts, p.Label+"  ("+id+")")
        }
        idx := tui.Choice("Sign in to which provider?", opts, 1)
        id := ids[idx-1]
        if err := LoginFlow(cfg, tui, id); err != nil {
                return err
        }
        cfg.EnsureProvider(id, "")
        if preset, _ := FindPreset(id); preset.DefaultModel != "" && cfg.ActiveModel == "" {
                cfg.ActiveModel = preset.DefaultModel
        }
        return finishSetup(cfg, tui, cfg.ActiveProvider)
}

// wizardLocalProvider configures Ollama / llama.cpp / KoboldCpp.
func wizardLocalProvider(cfg *Config, tui *Terminal, sel int) error {
        presets := localPresetsForWizard()
        idx := sel - 4
        if idx < 0 || idx >= len(presets) {
                return fmt.Errorf("invalid local provider")
        }
        preset := presets[idx]
        pcfg := cfg.EnsureProvider(preset.ID, preset.Kind)
        cfg.ActiveProvider = preset.ID

        // detect running instance
        stop := tui.Spinner("detecting " + preset.Label)
        ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
        err := CheckLocalServer(ctx, preset.BaseURL)
        cancel()
        stop()
        if err == nil {
                tui.Success("found a running server at " + preset.BaseURL)
        } else {
                custom := strings.TrimSpace(mustReadLine(tui, fmt.Sprintf("Server URL [%s]: ", preset.BaseURL)))
                if custom != "" {
                        pcfg.BaseURL = custom
                }
        }

        // model detection
        if preset.ID == "ollama" {
                ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
                models, err := DetectOllama(ctx, pcfg.BaseURL)
                cancel()
                if err == nil && len(models) > 0 {
                        opts := make([]string, 0, len(models)+1)
                        for _, m := range models {
                                opts = append(opts, fmt.Sprintf("%s  (%d MB %s %s)", m.Name, m.SizeMB, m.Quant, m.Details))
                        }
                        opts = append(opts, "Type a model name manually")
                        mi := tui.Choice("Detected local models — pick one:", opts, 1)
                        if mi <= len(models) {
                                cfg.ActiveModel = models[mi-1].Name
                        } else {
                                cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model name: "))
                        }
                } else {
                        tui.Warn("no local models detected — pull one with `ollama pull <model>` or type the name")
                        cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model name: "))
                }
        } else {
                cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model label ["+preset.DefaultModel+"]: "))
                if cfg.ActiveModel == "" {
                        cfg.ActiveModel = preset.DefaultModel
                }
        }
        pcfg.Auth = "none"
        return finishSetup(cfg, tui, preset.ID)
}

// wizardGGUF imports a GGUF file and starts a llama.cpp-compatible runtime.
func wizardGGUF(cfg *Config, tui *Terminal) error {
        tui.Block("GGUF models", "rup.ai can import GGUF files and serve them through a llama.cpp-\n" +
                "compatible runtime (llama-server / llamafile) if one is installed on PATH.\n" +
                "Nothing is downloaded automatically.")
        path := strings.TrimSpace(mustReadLine(tui, "Path to a .gguf file (empty to skip): "))
        if path == "" {
                tui.Info("skipped — you can import later with `rup.ai models --import <file>`")
                cfg.Onboarded = false
                return cfg.Save()
        }
        abs := path
        if !filepath.IsAbs(abs) {
                wd, _ := os.Getwd()
                abs = filepath.Join(wd, path)
        }
        stop := tui.Spinner("reading model metadata")
        info, err := ParseGGUF(abs)
        stop()
        if err != nil {
                return fmt.Errorf("cannot read GGUF: %v", err)
        }
        tui.Block("Model", fmt.Sprintf("file:     %s\nsize:     %s\nquant:    %s\narch:     %s\ncontext:  %d",
                filepath.Base(abs), humanBytes(uint64(info.SizeBytes)), info.Quant, info.Arch, info.CtxLength))
        if ram, err := SystemRAMBytes(); err == nil {
                need := uint64(EstimateRAM(info))
                line := fmt.Sprintf("estimated runtime memory: %s / system: %s", humanBytes(need), humanBytes(ram))
                if need > ram {
                        tui.Warn(line)
                        tui.Warn("this model will probably not fit — choose a smaller quantization")
                } else {
                        tui.Info(line)
                }
        }
        imported, err := ImportGGUF(abs, "")
        if err != nil {
                return err
        }
        tui.Success("imported to " + imported)

        if _, err := FindLlamaRuntime(); err != nil {
                tui.Warn(err.Error())
                tui.Block("What to do", "Install llama.cpp (https://github.com/ggml-org/llama.cpp) so that\n" +
                        "`llama-server` is on your PATH, then run `rup.ai models --serve <file.gguf>`.\n" +
                        "Cloud providers keep working without any local runtime.")
                cfg.Onboarded = false
                return cfg.Save()
        }
        port := 8081
        tui.Info("starting local server on 127.0.0.1:" + strconv.Itoa(port) + " …")
        mp, err := StartGGUFServer(context.Background(), imported, 4096, 0, 0, port)
        if err != nil {
                return err
        }
        time.Sleep(1200 * time.Millisecond)
        pcfg := cfg.EnsureProvider("llamacpp", "openai")
        pcfg.BaseURL = mp.BaseURL
        pcfg.Auth = "none"
        cfg.ActiveProvider = "llamacpp"
        cfg.ActiveModel = filepath.Base(imported)
        return finishSetup(cfg, tui, "llamacpp")
}

// loginIfNeeded stores credentials appropriate to the provider kind.
func loginIfNeeded(cfg *Config, tui *Terminal, providerID string, pcfg *ProviderConfig) error {
        switch pcfg.Auth {
        case "none":
                return nil
        }
        if APIKey(providerID) != "" {
                tui.Success("API key found in the credential store (" + providerID + ")")
                return nil
        }
        if env := os.Getenv("RUPAI_API_KEY"); env != "" {
                tui.Info("using RUPAI_API_KEY from the environment")
                return nil
        }
        return LoginFlow(cfg, tui, providerID)
}

func testAndReport(cfg *Config, providerID string) error {
        prov, err := buildProvider(cfg, providerID)
        if err != nil {
                return err
        }
        ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
        defer cancel()
        return TestConnection(ctx, prov)
}

func finishSetup(cfg *Config, tui *Terminal, providerID string) error {
        cfg.ActiveProvider = providerID
        cfg.Onboarded = true
        if err := cfg.Save(); err != nil {
                return err
        }
        tui.Success("configuration saved to the user config directory (credentials in the OS keyring)")
        WelcomeScreen(cfg, tui)
        return nil
}

// WelcomeScreen shows the essential commands after setup.
func WelcomeScreen(cfg *Config, tui *Terminal) {
        tui.Section("You're ready")
        tui.Block("Start building", "  rup.ai \"Build a REST API with SQLite and tests\"   # one-shot task\n" +
                "  rup.ai                                            # interactive session")
        tui.Block("Essential commands",
                "  rup.ai setup        re-run this wizard\n" +
                        "  rup.ai providers    list configured providers\n" +
                        "  rup.ai models       discover models\n" +
                        "  rup.ai login        add or refresh credentials\n" +
                        "  rup.ai logout       remove credentials\n" +
                        "  rup.ai config       interactive settings\n" +
                        "  rup.ai doctor       environment health check\n" +
                        "  rup.ai --help       all commands")
        tui.Block("Inside a session",
                "  /help /plan /code /agent /review /debug   switch modes\n" +
                        "  /provider /model /settings                switch AI without restarting\n" +
                        "  /files /diff /undo /usage /compact        track your changes\n" +
                        "  /preview                                  live app preview (web)\n" +
                        "  @path/to/file                             attach a file to your prompt")
}

// buildProvider constructs a Provider from active config + stored key.
func buildProvider(cfg *Config, providerID string) (Provider, error) {
        pcfg, ok := cfg.Providers[providerID]
        if !ok {
                return nil, fmt.Errorf("provider %q is not configured", providerID)
        }
        key := APIKey(providerID)
        if pcfg.Auth == "oauth" && key == "" {
                key = LoadSecret(providerID, "oauth_token")
        }
        return NewProvider(providerID, pcfg, key)
}

// SettingsUI is the interactive settings interface (/settings, rup.ai config).
func SettingsUI(cfg *Config, tui *Terminal) error {
        for {
                p, _ := cfg.Active()
                tui.Section("rup.ai settings")
                tui.Block("Current", fmt.Sprintf(
                        "provider:      %s\nmodel:         %s\nbase url:      %s\napproval:      %s\nsteps:         %d\ncontext budget:%d tokens\nweb ui:        %s:%d\nprofile:       %s",
                        cfg.ActiveProvider, cfg.ActiveModel, p.BaseURL, cfg.Policies.Approval,
                        cfg.Policies.MaxAgentSteps, cfg.Context.BudgetTokens, cfg.Web.Bind, cfg.Web.Port, cfg.ActiveProfile))
                sel := tui.Choice("Change:", []string{
                        "Switch provider", "Switch model", "API keys / login", "Approval policy",
                        "Agent limits (steps, timeouts)", "Context budget", "Web UI (enable/port)",
                        "Profiles", "Test connection", "Export settings (no secrets)", "Back"}, 11)
                switch sel {
                case 1:
                        // display order and mapping come from one helper, so
                        // the picked index always matches the shown line
                        opts, order := ProviderMenu(cfg)
                        idx := tui.Choice("Provider:", opts, 1)
                        if idx < 1 || idx > len(order) {
                                return fmt.Errorf("invalid choice")
                        }
                        id := order[idx-1]
                        if _, okc := cfg.Providers[id]; !okc {
                                cfg.EnsureProvider(id, "")
                        }
                        cfg.ActiveProvider = id
                        pcfg := cfg.Providers[id]
                        if pcfg.Model != "" {
                                cfg.ActiveModel = pcfg.Model
                        }
                case 2:
                        prov, err := buildProvider(cfg, cfg.ActiveProvider)
                        var models []string
                        if err == nil {
                                stop := tui.Spinner("fetching models")
                                ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
                                models, _ = prov.ListModels(ctx)
                                cancel()
                                stop()
                        }
                        if len(models) > 0 {
                                shown := models
                                if len(shown) > 40 {
                                        shown = shown[:40]
                                }
                                opts := append(shown, "Type a model id")
                                mi := tui.Choice("Model:", opts, 1)
                                if mi <= len(shown) {
                                        cfg.ActiveModel = shown[mi-1]
                                } else {
                                        cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model id: "))
                                }
                        } else {
                                cfg.ActiveModel = strings.TrimSpace(mustReadLine(tui, "Model id: "))
                        }
                case 3:
                        id := strings.TrimSpace(mustReadLine(tui, "Provider id ["+cfg.ActiveProvider+"]: "))
                        if id == "" {
                                id = cfg.ActiveProvider
                        }
                        if err := LoginFlow(cfg, tui, id); err != nil {
                                tui.Error(err.Error())
                        }
                case 4:
                        pol := tui.Choice("Approval policy:", []string{
                                "restricted — confirm every write and every command",
                                "standard   — confirm risky actions (recommended)",
                                "permissive — confirm only dangerous actions"}, 2)
                        cfg.Policies.Approval = []string{"restricted", "standard", "permissive"}[pol-1]
                case 5:
                        steps := strings.TrimSpace(mustReadLine(tui, fmt.Sprintf("Max agent steps [%d]: ", cfg.Policies.MaxAgentSteps)))
                        if n, err := strconv.Atoi(steps); err == nil && n > 0 && n <= 200 {
                                cfg.Policies.MaxAgentSteps = n
                        }
                        mins := strings.TrimSpace(mustReadLine(tui, fmt.Sprintf("Agent timeout minutes [%d]: ", cfg.Policies.AgentTimeoutMinutes)))
                        if n, err := strconv.Atoi(mins); err == nil && n > 0 {
                                cfg.Policies.AgentTimeoutMinutes = n
                        }
                case 6:
                        budget := strings.TrimSpace(mustReadLine(tui, fmt.Sprintf("Context budget in tokens [%d]: ", cfg.Context.BudgetTokens)))
                        if n, err := strconv.Atoi(budget); err == nil && n >= 2000 {
                                cfg.Context.BudgetTokens = n
                        }
                case 7:
                        cfg.Web.Enabled = tui.Confirm("Enable companion web UI on /preview?", cfg.Web.Enabled)
                        port := strings.TrimSpace(mustReadLine(tui, fmt.Sprintf("Web UI port [%d]: ", cfg.Web.Port)))
                        if n, err := strconv.Atoi(port); err == nil && n > 0 {
                                cfg.Web.Port = n
                        }
                case 8:
                        act := tui.Choice("Profiles:", []string{"Save current selection as profile", "Use a profile", "Back"}, 3)
                        if act == 1 {
                                name := strings.TrimSpace(mustReadLine(tui, "Profile name: "))
                                if name != "" {
                                        if err := cfg.SaveProfile(name); err != nil {
                                                tui.Error(err.Error())
                                        } else {
                                                tui.Success("profile saved: " + name)
                                        }
                                }
                        } else if act == 2 {
                                names := make([]string, 0, len(cfg.Profiles))
                                for n := range cfg.Profiles {
                                        names = append(names, n)
                                }
                                sort.Strings(names)
                                if len(names) == 0 {
                                        tui.Info("no profiles saved yet")
                                } else {
                                        idx := tui.Choice("Use profile:", names, 1)
                                        if err := cfg.UseProfile(names[idx-1]); err != nil {
                                                tui.Error(err.Error())
                                        }
                                }
                        }
                case 9:
                        if err := testAndReport(cfg, cfg.ActiveProvider); err != nil {
                                tui.Error("connection failed: " + err.Error())
                        } else {
                                tui.Success("connection OK")
                        }
                case 10:
                        data, err := ExportNonSensitive(cfg)
                        if err != nil {
                                tui.Error(err.Error())
                        } else {
                                tui.Block("Settings export (secrets excluded)", string(data))
                        }
                default:
                        return cfg.Save()
                }
        }
}

// Doctor runs environment checks and prints a health table.
func Doctor(cfg *Config, tui *Terminal) {
        tui.Section("rup.ai doctor")
        type check struct{ name, status, hint string }
        var checks []check

        if cfg.Configured() {
                checks = append(checks, check{"provider configured", "ok", cfg.ActiveProvider + " / " + cfg.ActiveModel})
                p, pok := cfg.Active()
                if APIKey(cfg.ActiveProvider) == "" && (!pok || p.Auth != "none") {
                        checks = append(checks, check{"credentials", "warn", "no API key found — run `rup.ai login`"})
                } else {
                        checks = append(checks, check{"credentials", "ok", "stored in OS credential store"})
                }
                stop := tui.Spinner("reaching provider")
                err := testAndReport(cfg, cfg.ActiveProvider)
                stop()
                if err != nil {
                        checks = append(checks, check{"provider reachable", "fail", err.Error()})
                } else {
                        checks = append(checks, check{"provider reachable", "ok", ""})
                }
        } else {
                checks = append(checks, check{"provider configured", "fail", "run `rup.ai setup`"})
        }

        if _, err := exec.LookPath("git"); err == nil {
                checks = append(checks, check{"git", "ok", ""})
        } else {
                checks = append(checks, check{"git", "warn", "git not found — version control tools disabled"})
        }
        if cfgP, err := ConfigPath(); err == nil {
                if _, err := os.Stat(filepath.Dir(cfgP)); err == nil {
                        checks = append(checks, check{"config directory", "ok", filepath.Dir(cfgP)})
                } else {
                        checks = append(checks, check{"config directory", "warn", "not created yet"})
                }
        }
        if dir, err := DataDir(); err == nil {
                checks = append(checks, check{"data directory", "ok", dir})
        }
        if _, err := SystemRAMBytes(); err == nil {
                checks = append(checks, check{"system memory probe", "ok", ""})
        } else {
                checks = append(checks, check{"system memory probe", "warn", "RAM unknown; GGUF checks approximate"})
        }
        if _, err := FindLlamaRuntime(); err == nil {
                checks = append(checks, check{"llama.cpp runtime", "ok", "found on PATH"})
        } else {
                checks = append(checks, check{"llama.cpp runtime", "info", "not installed (optional; cloud providers unaffected)"})
        }
        if _, err := exec.LookPath("node"); err == nil {
                checks = append(checks, check{"node", "ok", ""})
        } else {
                checks = append(checks, check{"node", "info", "not found (needed only for JS projects)"})
        }
        checks = append(checks, check{"keyring", "warn-or-ok", "keyring used when available; 0600 file fallback otherwise"})

        rows := make([][]string, 0, len(checks))
        for _, c := range checks {
                mark := "·"
                color := ansiGray
                switch {
                case c.status == "ok":
                        mark, color = "✔", ansiGreen
                case c.status == "warn":
                        mark, color = "!", ansiYellow
                case c.status == "fail":
                        mark, color = "✗", ansiRed
                }
                rows = append(rows, []string{tui.c(color, mark), c.name, c.status, c.hint})
        }
        tui.Table([]string{"", "check", "status", "detail"}, rows)
        tui.print("\n")
}

// mustReadLine is ReadLine that fails fast on EOF.
func mustReadLine(tui *Terminal, prompt string) string {
        line, err := tui.ReadLine(prompt)
        if err != nil {
                return ""
        }
        return line
}
