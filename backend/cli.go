package main

// cli.go — command routing and the interactive session: REPL loop, slash
// commands, mode switching, provider/model switching without restart, and
// the one-shot non-interactive mode used by scripts and tests.

import (
        "bufio"
        context "context"
        "fmt"
        "io"
        "os"
        "os/exec"
        "os/signal"
        "runtime"
        "sort"
        "strings"
        "sync/atomic"
        "time"
)

// Run is the entry point; returns the process exit code.
func Run(args []string) int {
        cfg, err := LoadConfig()
        if err != nil {
                fmt.Fprintf(os.Stderr, "warning: %v\n", err)
        }
        tui := NewTerminal(cfg)

        if len(args) == 0 {
                return runDefault(cfg, tui)
        }
        cmd, rest := args[0], args[1:]
        switch cmd {
        case "setup":
                if err := RunWizard(cfg, tui); err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                return 0
        case "providers":
                return cmdProviders(cfg, tui)
        case "models":
                return cmdModels(cfg, tui, rest)
        case "login":
                return cmdLogin(cfg, tui, rest)
        case "logout":
                return cmdLogout(cfg, tui, rest)
        case "config":
                if len(rest) > 0 && rest[0] == "export" {
                        data, err := ExportNonSensitive(cfg)
                        if err != nil {
                                tui.Error(err.Error())
                                return 1
                        }
                        fmt.Println(string(data))
                        return 0
                }
                if err := SettingsUI(cfg, tui); err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                return 0
        case "doctor":
                Doctor(cfg, tui)
                return 0
        case "preview":
                return cmdPreviewServe(cfg, tui)
        case "version", "--version", "-v":
                fmt.Printf("rup.ai %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
                return 0
        case "help", "--help", "-h":
                printHelp(tui)
                return 0
        default:
                // treat everything as a one-shot objective
                return runOneShot(cfg, tui, strings.Join(args, " "))
        }
}

// runDefault launches the interactive session (wizard first, then REPL).
func runDefault(cfg *Config, tui *Terminal) int {
        if NeedsSetup(cfg) {
                tui.Banner(cfg.UI.Animation)
                if err := RunWizard(cfg, tui); err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                if !cfg.Configured() {
                        return 0 // user chose "configure later"
                }
        } else {
                tui.Banner(cfg.UI.Animation)
        }
        sess, err := newSession(cfg, tui)
        if err != nil {
                tui.Error(err.Error())
                return 1
        }
        return sess.runSession()
}

// runOneShot executes a single objective non-interactively.
func runOneShot(cfg *Config, tui *Terminal, objective string) int {
        objective = strings.TrimSpace(objective)
        if objective == "" {
                // accept piped stdin as the objective
                if !tui.isTTY {
                        raw, _ := io.ReadAll(bufio.NewReader(os.Stdin))
                        objective = strings.TrimSpace(string(raw))
                }
        }
        if objective == "" {
                printHelp(tui)
                return 2
        }
        if !cfg.Configured() {
                tui.Error("rup.ai is not configured yet. Run `rup.ai setup` first.")
                return 1
        }
        sess, err := newSession(cfg, tui)
        if err != nil {
                tui.Error(err.Error())
                return 1
        }
        return sess.runObjective(objective, false)
}

// printHelp lists every command.
func printHelp(tui *Terminal) {
        tui.print(tui.c(ansiBold+ansiCyan, "rup.ai") + tui.c(ansiGray, " — terminal-first AI coding agent") + "\n\n")
        tui.Block("Usage",
                "  rup.ai                          interactive session (first run: setup wizard)\n" +
                        "  rup.ai \"build a todo API\"       run one objective and exit\n" +
                        "  rup.ai <command> [args]         run a command")
        tui.Block("Commands",
                "  setup       interactive first-launch setup wizard\n" +
                        "  providers   list configured and available providers\n" +
                        "  models      discover models (list/import/serve GGUF)\n" +
                        "  login       store credentials for a provider\n" +
                        "  logout      remove stored credentials\n" +
                        "  config      interactive settings (config export to print without secrets)\n" +
                        "  doctor      environment health check\n" +
                        "  preview     start the localhost companion web UI\n" +
                        "  version     print version")
        tui.Block("Flags",
                "  --provider <id>   override provider for this run\n" +
                        "  --model <id>      override model for this run\n" +
                        "  --cwd <dir>       set the workspace\n" +
                        "  --mode <mode>     ask|plan|code|agent|review|debug\n" +
                        "  --no-anim         skip the startup animation\n" +
                        "  --plain           disable colors and animation")
        tui.Block("Docs", "README.md covers providers, authentication and security policies.")
}

// --------------------------------------------------------------------------
// session
// --------------------------------------------------------------------------

type session struct {
        cfg     *Config
        tui     *Terminal
        ws      *Workspace
        prov    Provider
        reg     *ToolRegistry
        servers *ServerManager
        agent   *Agent
        changes *ChangeSet
        webURL  string
        webStop func()
        webUp   atomic.Bool
        busy    atomic.Bool
}

// newSession wires provider, workspace, tools and the agent.
func newSession(cfg *Config, tui *Terminal) (*session, error) {
        cwd := os.Getenv("RUPAI_CWD")
        ws, err := ResolveWorkspace(cwd)
        if err != nil {
                return nil, err
        }
        if !cfg.Configured() {
                return nil, fmt.Errorf("no provider configured; run `rup.ai setup`")
        }
        prov, err := buildProvider(cfg, cfg.ActiveProvider)
        if err != nil {
                return nil, err
        }
        s := &session{cfg: cfg, tui: tui, ws: ws, prov: prov, changes: &ChangeSet{}}
        s.servers = NewServerManager(ws)
        s.reg = NewToolRegistry(ws, cfg, s.changes, s.servers, s.approve, tui.Out)
        s.agent = NewAgent(cfg, prov, ws, s.reg, tui, s.changes)
        if m, err := ParseMode(os.Getenv("RUPAI_MODE")); err == nil {
                s.agent.Mode = m
        }
        return s, nil
}

// approve is the interactive approval callback used by tools.
func (s *session) approve(title, detail string, risk Risk) bool {
        color := ansiYellow
        if risk == RiskDangerous {
                color = ansiRed
        }
        s.tui.printf("\n%s %s\n", s.tui.c(color, "approval needed:"), s.tui.c(ansiBold, title))
        if detail != "" && detail != title {
                s.tui.print("  " + s.tui.c(ansiText, detail) + "\n")
        }
        ok := s.tui.Confirm("  allow this action?", false)
        if ok {
                s.tui.print(s.tui.c(ansiGreen, "  allowed") + "\n")
        } else {
                s.tui.print(s.tui.c(ansiRed, "  denied") + "\n")
        }
        return ok
}

// banner shows provider/model/workspace header.
func (s *session) banner() {
        p, _ := s.cfg.Active()
        mode := s.agent.Mode
        s.tui.StatusLine(s.cfg.ActiveProvider, s.agent.model(), string(mode), s.ws.Root)
        _ = p
        s.tui.Info("type an objective, or /help for commands")
}

// runObjective executes one objective and prints the summary. Returns exit code.
func (s *session) runObjective(objective string, interactive bool) int {
        s.busy.Store(true)
        defer s.busy.Store(false)

        ctx, cancel := context.WithCancel(context.Background())
        sigCh := make(chan os.Signal, 2)
        signal.Notify(sigCh, os.Interrupt)
        defer signal.Stop(sigCh)

        go func() {
                for range sigCh {
                        if s.busy.Load() {
                                s.tui.print("\n" + s.tui.c(ansiYellow, "  cancelling after the current step… (press Ctrl+C again to force)") + "\n")
                                cancel()
                        } else {
                                fmt.Println()
                                os.Exit(130)
                        }
                }
        }()

        started := time.Now()
        final, err := s.agent.Run(ctx, objective)
        elapsed := time.Since(started).Round(time.Second)
        if err != nil && err != context.Canceled {
                s.tui.Error(err.Error())
        }
        ok := err == nil || err == context.Canceled
        s.tui.print("\n")
        s.tui.print(s.tui.c(ansiBold, "Task "+map[bool]string{true: "completed", false: "stopped"}[ok]) +
                s.tui.c(ansiGray, "  ("+elapsed.String()+", step "+fmt.Sprint(s.agent.Steps)+", "+
                        fmt.Sprint(s.agent.Usage.Total())+" tokens)") + "\n")
        s.tui.print(s.tui.c(ansiText, s.agent.SessionSummary()) + "\n")
        if final != "" {
                s.tui.print("\n" + s.tui.c(ansiGray, strings.Repeat("─", min(50, s.tui.Width-2))) + "\n")
                for _, line := range strings.Split(final, "\n") {
                        s.tui.print(s.tui.renderInline(line) + "\n")
                }
        }
        return map[bool]int{true: 0, false: 1}[ok]
}

// runSession is the main REPL loop.
func (s *session) runSession() int {
        s.banner()
        // use the SAME buffered reader as all prompt helpers (ReadLine,
        // Choice, Confirm) — a second bufio.Reader here would swallow typed
        // lines into its own buffer and desynchronize every following prompt
        reader := stdinReader
        for {
                prompt := s.tui.c(ansiViolet+ansiBold, "rup.ai") + s.tui.c(ansiGray, " ❯ ")
                s.tui.print(prompt)
                line, err := reader.ReadString('\n')
                if err != nil {
                        // EOF or Ctrl+C at idle: leave cleanly
                        s.tui.print("\n")
                        s.shutdownWeb()
                        return 0
                }
                input := strings.TrimSpace(line)
                if input == "" {
                        continue
                }
                if strings.HasPrefix(input, "/") {
                        quit := s.slash(input)
                        if quit {
                                s.shutdownWeb()
                                return 0
                        }
                        continue
                }
                if !s.cfg.Configured() {
                        s.tui.Error("provider missing; run /provider or `rup.ai setup`")
                        continue
                }
                s.runObjective(input, true)
        }
}

// slash handles interactive commands; returns true when the session should end.
func (s *session) slash(input string) bool {
        name, rest := input, ""
        if i := strings.IndexAny(input, " "); i >= 0 {
                name, rest = input[:i], strings.TrimSpace(input[i+1:])
        }
        switch name {
        case "/help":
                s.tui.Section("Session commands")
                s.tui.Block("Modes", "  /plan <objective>   produce an implementation plan first\n"+
                        "  /approve            approve the pending plan and implement it\n"+
                        "  /reject             discard the pending plan\n"+
                        "  /mode <m>           ask|plan|code|agent|review|debug")
                s.tui.Block("AI setup", "  /provider           switch provider\n"+
                        "  /model              switch model\n"+
                        "  /login              add credentials\n"+
                        "  /settings           interactive settings")
                s.tui.Block("Changes & state", "  /files              files changed this session\n"+
                        "  /diff               show diffs of this session's changes\n"+
                        "  /undo               restore the last backup\n"+
                        "  /usage              token usage\n"+
                        "  /status             session status\n"+
                        "  /compact            summarize and shrink the conversation")
                s.tui.Block("Tools", "  /terminal           open your shell inside the workspace\n"+
                        "  /preview            start/show the localhost web preview\n"+
                        "  /clear              reset conversation\n"+
                        "  /exit               quit (Ctrl+C also works)")
                return false

        case "/exit", "/quit":
                s.tui.Info("goodbye")
                return true

        case "/clear":
                s.agent.History = nil
                s.agent.Usage = Usage{}
                s.tui.Success("conversation cleared")
                return false

        case "/mode":
                if rest == "" {
                        s.tui.Info("modes: ask, plan, code, agent, review, debug — usage: /mode agent")
                        return false
                }
                m, err := ParseMode(rest)
                if err != nil {
                        s.tui.Error(err.Error())
                        return false
                }
                s.agent.Mode = m
                s.tui.Success("mode: " + string(m))
                return false

        case "/plan":
                objective := rest
                if objective == "" {
                        objective = strings.TrimSpace(mustReadLine(s.tui, "objective:"))
                }
                if objective == "" {
                        s.tui.Warn("usage: /plan <objective>")
                        return false
                }
                s.tui.Info("planning — no files will be modified")
                if _, err := s.agent.PlanFlow(s.runCtx(), objective); err != nil {
                        s.tui.Error(err.Error())
                        return false
                }
                s.tui.print(s.tui.c(ansiYellow, "\nPlan ready. /approve to implement, /reject to discard.\n"))
                return false

        case "/approve":
                if s.agent.Plan == "" {
                        s.tui.Warn("no pending plan; use /plan <objective> first")
                        return false
                }
                s.agent.Mode = ModeAgent
                s.agent.History = append(s.agent.History,
                        Message{Role: RoleUser, Content: "The user approved this plan. Implement it now:\n" + s.agent.Plan})
                s.agent.Plan = ""
                s.runObjective("implement the approved plan above", true)
                return false

        case "/reject":
                s.agent.Plan = ""
                s.tui.Success("plan discarded")
                return false

        case "/provider":
                s.cmdProviderSwitch()
                return false

        case "/model":
                s.cmdModelSwitch()
                return false

        case "/login":
                id := rest
                if id == "" {
                        id = s.cfg.ActiveProvider
                }
                if err := LoginFlow(s.cfg, s.tui, id); err != nil {
                        s.tui.Error(err.Error())
                }
                return false

        case "/logout":
                if err := Logout(s.cfg, s.tui, rest); err != nil {
                        s.tui.Error(err.Error())
                }
                return false

        case "/settings":
                if err := SettingsUI(s.cfg, s.tui); err != nil {
                        s.tui.Error(err.Error())
                }
                s.rewire()
                return false

        case "/files":
                files := s.agent.ChangedFiles()
                if len(files) == 0 {
                        s.tui.Info("no files changed this session")
                } else {
                        s.tui.Section("Changed files")
                        for _, f := range files {
                                s.tui.print("  " + f + "\n")
                        }
                }
                return false

        case "/diff":
                shown := false
                s.changes.mu.Lock()
                diffs := append([]string{}, s.changes.Diffs...)
                s.changes.mu.Unlock()
                for _, d := range diffs {
                        s.tui.RenderDiff(d + "\n")
                        shown = true
                }
                if !shown {
                        s.tui.Info("no diffs recorded this session (try git_diff via the agent, or /terminal git diff)")
                }
                return false

        case "/undo":
                out, err := s.reg.Call(s.runCtx(), "restore_backup", "{}")
                if err != nil {
                        s.tui.Error(err.Error())
                } else {
                        s.tui.Success(out)
                }
                return false

        case "/usage":
                s.tui.Info(fmt.Sprintf("session tokens: %d in / %d out  ·  steps: %d",
                        s.agent.Usage.InputTokens, s.agent.Usage.OutputTokens, s.agent.Steps))
                return false

        case "/status":
                p, pok := s.cfg.Active()
                kind := ""
                if pok {
                        kind = p.Kind
                }
                s.tui.Section("Status")
                s.tui.Block("Session", fmt.Sprintf(
                        "provider:   %s (%s)\nmodel:      %s\nmode:       %s\nworkspace:  %s\nsteps:      %d\ntokens:     %d\nservers:    %d\nweb ui:     %s",
                        s.cfg.ActiveProvider, kind, s.agent.model(), s.agent.Mode, s.ws.Root,
                        s.agent.Steps, s.agent.Usage.Total(), len(s.servers.List()), s.webStatus()))
                return false

        case "/compact":
                s.tui.Info("compacting…")
                s.forceCompact()
                return false

        case "/terminal":
                s.tui.Info("opening your shell — type `exit` to return to rup.ai (RUPAI_CHILD=1 is set)")
                shell, args := ShellForOS()
                if isWindows() && os.Getenv("ComSpec") != "" && !strings.Contains(strings.ToLower(shell), "powershell") {
                        // fall back to an interactive PowerShell when available
                        if _, err := exec.LookPath("powershell"); err == nil {
                                shell, args = "powershell", []string{"-NoLogo"}
                        }
                }
                _ = args
                cmd := exec.Command(shell)
                if !isWindows() {
                        cmd = exec.Command(shell, "-i")
                }
                cmd.Env = append(os.Environ(), "RUPAI_CHILD=1")
                cmd.Dir = s.ws.Root
                cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
                if err := cmd.Run(); err != nil {
                        s.tui.Warn("shell exited: " + err.Error())
                }
                return false

        case "/preview":
                s.cmdPreview(rest)
                return false

        default:
                s.tui.Warn("unknown command " + name + " — /help lists everything")
                return false
        }
}

// runCtx returns a fresh cancellable context for tool-only operations.
func (s *session) runCtx() context.Context { return context.Background() }

// forceCompact summarizes older history unconditionally.
func (s *session) forceCompact() {
        if len(s.agent.History) < 4 {
                s.tui.Info("conversation is already small")
                return
        }
        split := len(s.agent.History) - 2
        summary, err := s.agent.summarize(s.runCtx(), s.agent.History[:split])
        if err != nil {
                s.tui.Warn("provider summarize failed: " + err.Error())
                summary = "(history truncated by /compact)"
        }
        compacted := make([]Message, 0, 3)
        compacted = append(compacted, Message{Role: RoleUser, Content: "Conversation summary:\n" + summary})
        compacted = append(compacted, s.agent.History[split:]...)
        s.agent.History = compacted
        s.tui.Success(fmt.Sprintf("compacted: %d messages retained", len(s.agent.History)))
}

// cmdProviderSwitch changes provider without restarting. The menu order and
// the index→id mapping both come from ProviderMenu, so the number you type
// always selects the line you saw.
func (s *session) cmdProviderSwitch() {
        opts, order := ProviderMenu(s.cfg)
        idx := s.tui.Choice("Switch provider to:", opts, 1)
        if idx < 1 || idx > len(order) {
                s.tui.Error("invalid selection")
                return
        }
        chosen := order[idx-1]
        if _, ok := s.cfg.Providers[chosen]; !ok {
                // unconfigured preset chosen: configure it inline (guided, no restart)
                preset, _ := FindPreset(chosen)
                pcfg := s.cfg.EnsureProvider(chosen, preset.Kind)
                if pcfg.BaseURL == "" {
                        pcfg.BaseURL = preset.BaseURL
                }
                if err := loginIfNeeded(s.cfg, s.tui, chosen, pcfg); err != nil {
                        s.tui.Error(err.Error())
                        return
                }
        }
        prov, err := buildProvider(s.cfg, chosen)
        if err != nil {
                s.tui.Error(err.Error())
                return
        }
        s.prov = prov
        s.agent.prov = prov
        s.tui.Success("provider: " + chosen + " · model: " + s.agent.model())
        _ = s.cfg.Save()
}

// cmdModelSwitch changes model without restarting.
func (s *session) cmdModelSwitch() {
        stop := s.tui.Spinner("fetching models")
        ctx, cancel := context.WithTimeout(s.runCtx(), 20*time.Second)
        models, err := s.prov.ListModels(ctx)
        cancel()
        stop()
        if err != nil || len(models) == 0 {
                s.tui.Info("model discovery unavailable (" + fmt.Sprint(err) + ") — type the model id")
                m := strings.TrimSpace(mustReadLine(s.tui, "model id:"))
                if m != "" {
                        s.cfg.ActiveModel = m
                        _ = s.cfg.Save()
                        s.tui.Success("model: " + m)
                }
                return
        }
        shown := models
        if len(shown) > 30 {
                shown = shown[:30]
        }
        opts := append(shown, "type a model id manually")
        idx := s.tui.Choice("Model:", opts, 1)
        if idx <= len(shown) {
                s.cfg.ActiveModel = shown[idx-1]
        } else {
                m := strings.TrimSpace(mustReadLine(s.tui, "model id:"))
                if m == "" {
                        return
                }
                s.cfg.ActiveModel = m
        }
        _ = s.cfg.Save()
        s.tui.Success("model: " + s.cfg.ActiveModel)
}

// rewire rebuilds provider-dependent state after settings changes.
func (s *session) rewire() {
        prov, err := buildProvider(s.cfg, s.cfg.ActiveProvider)
        if err == nil {
                s.prov = prov
                s.agent.prov = prov
        }
}

// cmdPreview starts/shows/stops the companion web UI.
func (s *session) cmdPreview(rest string) {
        if rest == "stop" {
                s.shutdownWeb()
                s.tui.Success("web preview stopped")
                return
        }
        if s.webUp.Load() {
                s.tui.Info("web preview already running: " + s.webURL)
                _ = OpenInBrowser(s.webURL)
                return
        }
        bridge := &WebBridge{
                Status: func() StatusPayload {
                        return StatusPayload{
                                Version: version, Provider: s.cfg.ActiveProvider, Model: s.agent.model(),
                                Mode: string(s.agent.Mode), Workspace: s.ws.Root,
                                UptimeSec: int64(time.Since(s.agent.Started()).Seconds()),
                                UsageTokens: s.agent.Usage.Total(), Steps: s.agent.Steps,
                                Providers: s.cfg.ProviderIDs(),
                        }
                },
                Config: func() *Config { return s.cfg },
                SaveConfig: func(fn func(*Config)) error {
                        fn(s.cfg)
                        return s.cfg.Save()
                },
                Models: func(ctx context.Context, providerID string) ([]string, error) {
                        prov, err := buildProvider(s.cfg, providerID)
                        if err != nil {
                                return nil, err
                        }
                        return prov.ListModels(ctx)
                },
                Servers:      func() []DevServer { return s.servers.List() },
                ServerOutput: func(id string, n int) (string, bool) { return s.servers.Output(id, n) },
                StopServer:   func(id string) (bool, error) { return s.servers.Stop(id) },
                OpenBrowser:  func(url string) error { return OpenInBrowser(url) },
                Busy:         func() bool { return s.busy.Load() },
        }
        url, stop, err := ServeWeb(s.cfg, bridge)
        if err != nil {
                s.tui.Error(err.Error())
                return
        }
        s.webURL, s.webStop = url, stop
        s.webUp.Store(true)
        s.tui.Success("web preview: " + s.webURL)
        _ = OpenInBrowser(url)
}

func (s *session) webStatus() string {
        if s.webUp.Load() {
                return s.webURL
        }
        return "off (/preview to start)"
}

func (s *session) shutdownWeb() {
        if s.webUp.Load() && s.webStop != nil {
                s.webStop()
                s.webUp.Store(false)
        }
}

// Started helper on agent (timestamped at construction).
func (a *Agent) Started() time.Time { return a.startedAt }

// --------------------------------------------------------------------------
// top-level commands
// --------------------------------------------------------------------------

func cmdProviders(cfg *Config, tui *Terminal) int {
        if err := printProviders(cfg, tui); err != nil {
                tui.Error(err.Error())
                return 1
        }
        return 0
}

func printProviders(cfg *Config, tui *Terminal) error {
        tui.Section("Configured providers")
        rows := make([][]string, 0, len(cfg.Providers))
        for _, id := range cfg.ProviderIDs() {
                p := cfg.Providers[id]
                active := ""
                if id == cfg.ActiveProvider {
                        active = "← active"
                }
                rows = append(rows, []string{id, p.Kind, p.BaseURL, p.Model, active})
        }
        tui.Table([]string{"id", "kind", "base url", "model", ""}, rows)

        tui.Section("Available presets (rup.ai setup → choice 1-8)")
        rows = rows[:0]
        for _, p := range Presets {
                rows = append(rows, []string{p.ID, p.Label, p.Kind, map[bool]string{true: "local", false: "cloud"}[p.Local]})
        }
        tui.Table([]string{"id", "provider", "api kind", "scope"}, rows)
        tui.print("\n" + tui.c(ansiGray, "Subscription sign-in note: see `rup.ai login --info` or README (API keys are the official third-party route).") + "\n")
        return nil
}

func cmdModels(cfg *Config, tui *Terminal, rest []string) int {
        // flags: --provider id | --url u | --import file | --serve file [--gpu-layers n] [--ctx n] [--threads n]
        providerID := cfg.ActiveProvider
        customURL := ""
        importPath := ""
        servePath := ""
        gpuLayers := 0
        ctxLen := 4096
        threads := 0
        for i := 0; i < len(rest); i++ {
                switch rest[i] {
                case "--provider":
                        i++
                        if i < len(rest) {
                                providerID = rest[i]
                        }
                case "--url":
                        i++
                        if i < len(rest) {
                                customURL = rest[i]
                        }
                case "--import":
                        i++
                        if i < len(rest) {
                                importPath = rest[i]
                        }
                case "--serve":
                        i++
                        if i < len(rest) {
                                servePath = rest[i]
                        }
                case "--gpu-layers":
                        i++
                        if i < len(rest) {
                                gpuLayers, _ = parseIntSafe(rest[i])
                        }
                case "--ctx":
                        i++
                        if i < len(rest) {
                                ctxLen, _ = parseIntSafe(rest[i])
                        }
                case "--threads":
                        i++
                        if i < len(rest) {
                                threads, _ = parseIntSafe(rest[i])
                        }
                }
        }

        if importPath != "" {
                dest, err := ImportGGUF(importPath, "")
                if err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                info, _ := ParseGGUF(dest)
                tui.Success("imported " + dest)
                tui.Block("Metadata", fmt.Sprintf("size %s · quant %s · arch %s · context %d",
                        humanBytes(uint64(info.SizeBytes)), info.Quant, info.Arch, info.CtxLength))
                return 0
        }
        if servePath != "" {
                mp, err := StartGGUFServer(context.Background(), servePath, ctxLen, threads, gpuLayers, 8081)
                if err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                tui.Success("serving " + mp.Model + " at " + mp.BaseURL + " (Ctrl+C to stop)")
                sig := make(chan os.Signal, 1)
                signal.Notify(sig, os.Interrupt)
                <-sig
                StopGGUFServer()
                return 0
        }

        pcfg, ok := cfg.Providers[providerID]
        if !ok {
                preset, known := FindPreset(providerID)
                if !known || !preset.Local {
                        tui.Error("provider not configured: " + providerID)
                        return 1
                }
                pcfg = cfg.EnsureProvider(providerID, preset.Kind)
        }
        if customURL != "" {
                pcfg.BaseURL = customURL
        }
        ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
        defer cancel()

        var models []ModelInfo
        if providerID == "ollama" {
                ms, err := DetectOllama(ctx, pcfg.BaseURL)
                if err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                models = ms
        } else if providerID == "koboldcpp" {
                ms, err := DetectKobold(ctx, pcfg.BaseURL)
                if err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                models = ms
        } else {
                prov, err := buildProvider(cfg, providerID)
                if err != nil {
                        tui.Error(err.Error())
                        return 1
                }
                stop := tui.Spinner("discovering models")
                ids, err := prov.ListModels(ctx)
                stop()
                if err != nil {
                        tui.Error("model discovery failed: " + err.Error())
                        return 1
                }
                sort.Strings(ids)
                for _, id := range ids {
                        models = append(models, ModelInfo{Name: id, Provider: providerID})
                }
        }
        rows := make([][]string, 0, len(models))
        for _, m := range models {
                rows = append(rows, []string{m.Name, m.Provider,
                        map[bool]string{true: fmt.Sprint(m.SizeMB) + " MB", false: ""}[m.SizeMB > 0], m.Quant, m.Details})
        }
        tui.Section("Models — " + providerID)
        tui.Table([]string{"model", "provider", "size", "quant", "details"}, rows)
        return 0
}

func parseIntSafe(s string) (int, error) {
        n := 0
        for _, c := range s {
                if c < '0' || c > '9' {
                        return 0, fmt.Errorf("not a number")
                }
                n = n*10 + int(c-'0')
        }
        if n == 0 && s != "0" {
                return 0, fmt.Errorf("not a number")
        }
        return n, nil
}

func cmdLogin(cfg *Config, tui *Terminal, rest []string) int {
        if len(rest) > 0 && rest[0] == "--info" {
                tui.Section("Authentication reality check")
                tui.Block("Official subscription access", "ChatGPT (OpenAI) and Claude (Anthropic) subscription logins\n" +
                        "only work inside those companies' first-party apps. They do not\n" +
                        "authorize third-party CLI tools. rup.ai never impersonates official\n" +
                        "clients or extracts browser credentials.")
                tui.Block("What rup.ai uses", "1. API keys — officially supported by every cloud provider here.\n" +
                        "2. OAuth2 (device / PKCE) — used only when a provider or your own\n" +
                        "   gateway publishes third-party OAuth endpoints (configurable).\n" +
                        "3. Local servers (Ollama, llama.cpp, KoboldCpp) — no account needed.")
                return 0
        }
        id := ""
        if len(rest) > 0 {
                id = rest[0]
        }
        if id == "" {
                if cfg.ActiveProvider != "" {
                        id = cfg.ActiveProvider
                } else {
                        tui.Error("usage: rup.ai login <provider-id>   (rup.ai login --info for details)")
                        return 1
                }
        }
        if err := LoginFlow(cfg, tui, id); err != nil {
                tui.Error(err.Error())
                return 1
        }
        return 0
}

func cmdLogout(cfg *Config, tui *Terminal, rest []string) int {
        id := ""
        if len(rest) > 0 {
                id = rest[0]
        }
        if err := Logout(cfg, tui, id); err != nil {
                tui.Error(err.Error())
                return 1
        }
        return 0
}

// cmdPreviewServe is the standalone `rup.ai preview` command.
func cmdPreviewServe(cfg *Config, tui *Terminal) int {
        sess, err := newSession(cfg, tui)
        if err != nil {
                tui.Error(err.Error())
                return 1
        }
        sess.cmdPreview("")
        if !sess.webUp.Load() {
                return 1
        }
        tui.Info("press Ctrl+C to stop the web preview")
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, os.Interrupt)
        <-sig
        sess.shutdownWeb()
        return 0
}
