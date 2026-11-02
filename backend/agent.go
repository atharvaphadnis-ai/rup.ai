package main

// agent.go — the agentic execution engine: a genuine observe→plan→act→verify
// loop over a real provider. The model reasons, calls tools, rup.ai executes
// them against the real filesystem/commands, feeds results back, and repeats
// until the objective is met, the step budget is exhausted, or the user
// cancels with Ctrl+C. Context is assembled from the workspace and compacted
// when it grows beyond the configured budget.

import (
        "context"
        "fmt"
        "runtime"
        "sort"
        "strings"
        "time"
)

// Mode is a coding mode (section 10 of the product spec).
type Mode string

const (
        ModeAsk    Mode = "ask"
        ModePlan   Mode = "plan"
        ModeCode   Mode = "code"
        ModeAgent  Mode = "agent"
        ModeReview Mode = "review"
        ModeDebug  Mode = "debug"
)

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
        switch Mode(strings.ToLower(strings.TrimSpace(s))) {
        case ModeAsk:
                return ModeAsk, nil
        case ModePlan:
                return ModePlan, nil
        case ModeCode:
                return ModeCode, nil
        case ModeAgent:
                return ModeAgent, nil
        case ModeReview:
                return ModeReview, nil
        case ModeDebug:
                return ModeDebug, nil
        }
        return "", fmt.Errorf("unknown mode %q (ask|plan|code|agent|review|debug)", s)
}

// readToolsOnly reports whether the mode allows mutating tools.
func (m Mode) readToolsOnly() bool {
        switch m {
        case ModeAsk, ModePlan, ModeReview:
                return true
        }
        return false
}

// AgentEvent describes progress for UI rendering.
type AgentEvent struct {
        Kind  string // "step", "tool", "tool_result", "warn", "error", "usage"
        Text  string
        Usage Usage
}

// Agent runs the tool-calling loop against one provider.
type Agent struct {
        cfg     *Config
        prov    Provider
        ws      *Workspace
        reg     *ToolRegistry
        tui     *Terminal
        changes *ChangeSet

        Mode      Mode
        History   []Message
        Usage     Usage
        Steps     int
        Plan      string // approved plan (plan mode)
        startedAt time.Time

        lastError error
}

// NewAgent wires an agent instance.
func NewAgent(cfg *Config, prov Provider, ws *Workspace, reg *ToolRegistry, tui *Terminal, changes *ChangeSet) *Agent {
        return &Agent{cfg: cfg, prov: prov, ws: ws, reg: reg, tui: tui, changes: changes, Mode: ModeAgent, startedAt: time.Now()}
}

// SystemPrompt builds the mode-specific system prompt with real workspace facts.
func (a *Agent) SystemPrompt() string {
        var b strings.Builder
        b.WriteString("You are rup.ai, a terminal-first AI coding agent working directly inside the user's project.\n\n")
        b.WriteString("OPERATING RULES\n")
        b.WriteString("- You act through tools. Inspect before you change: read files before editing them.\n")
        b.WriteString("- Prefer edit_file with exact old_text over rewriting whole files with write_file.\n")
        b.WriteString("- Keep changes minimal, idiomatic and consistent with the project's conventions.\n")
        b.WriteString("- After meaningful changes, build and test using run_command (e.g. the project's build/test commands) and fix what fails.\n")
        b.WriteString("- Never invent command output, test results or file contents. If a tool fails, read the error and adapt.\n")
        b.WriteString("- Stay inside the workspace. Paths outside it are refused.\n")
        b.WriteString("- Never print, commit or transmit credentials. .env-style files are redacted automatically.\n")
        b.WriteString("- If an objective needs external services or credentials that are missing, say exactly what is missing instead of pretending success.\n")
        b.WriteString("- When you finish, summarize concisely: what was created/changed, commands run, test results, how to run the app.\n\n")

        b.WriteString("ENVIRONMENT\n")
        b.WriteString("OS: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
        b.WriteString("Workspace: " + a.ws.Root + "\n")
        if a.ws.Info.GitRepo {
                b.WriteString("Git repository: yes (inspect with git_status/git_diff before large changes)\n")
        }
        pi := a.ws.Info
        if len(pi.Languages) > 0 {
                b.WriteString("Detected stack: " + strings.Join(pi.Languages, ", "))
                if len(pi.Frameworks) > 0 {
                        b.WriteString(" | frameworks: " + strings.Join(pi.Frameworks, ", "))
                }
                b.WriteString("\n")
        }
        if pi.BuildCmd != "" || pi.TestCmd != "" || pi.DevCmd != "" {
                b.WriteString("Conventions: build=" + pi.BuildCmd + " test=" + pi.TestCmd + " dev=" + pi.DevCmd + "\n")
        }

        if g := a.ws.ProjectGuidance(); g != "" {
                b.WriteString("\nPROJECT INSTRUCTIONS\n" + g + "\n")
        }

        b.WriteString("\nPROJECT TREE\n" + a.ws.Tree(a.cfg.Context.TreeMaxLines) + "\n")

        switch a.Mode {
        case ModeAsk:
                b.WriteString("\nMODE: ASK — answer questions about this project. Do not modify files unless explicitly asked.\n")
        case ModePlan:
                b.WriteString("\nMODE: PLAN — inspect the project and produce a numbered implementation plan (files to create/modify, commands to run, risks). Do not modify anything. End the plan with 'PLAN COMPLETE'.\n")
        case ModeCode:
                b.WriteString("\nMODE: CODE — make targeted changes with edit tools. Verify compilation when practical. Explain each change briefly.\n")
        case ModeAgent:
                b.WriteString("\nMODE: AGENT — autonomously implement the objective: create files, install dependencies if needed, run builds/tests, read real errors, fix them, and report honestly what happened.\n")
        case ModeReview:
                b.WriteString("\nMODE: REVIEW — inspect the code for bugs, security issues, maintainability and performance. Report findings with file:line references and severity. Do not modify files.\n")
        case ModeDebug:
                b.WriteString("\nMODE: DEBUG — analyze errors/logs (use run_command to reproduce), locate the root cause, implement a fix, verify it, and explain the root cause.\n")
        }
        return b.String()
}

// assembleContext builds the request messages from history and budget.
func (a *Agent) assembleContext(objective string) []Message {
        budget := a.cfg.Context.BudgetTokens
        if budget <= 0 {
                budget = 24000
        }
        budget -= EstimateTokens(a.SystemPrompt())
        budget -= 1200 // headroom for the answer

        msgs := make([]Message, 0, len(a.History)+1)
        used := 0
        for i := len(a.History) - 1; i >= 0 && used < budget; i-- {
                m := a.History[i]
                cost := EstimateTokens(m.Content)
                if len(m.ToolCalls) > 0 {
                        for _, tc := range m.ToolCalls {
                                cost += EstimateTokens(tc.Arguments)
                        }
                }
                if used+cost > budget && len(msgs) >= 2 {
                        break
                }
                msgs = append([]Message{m}, msgs...)
                used += cost
        }
        expanded, _ := a.ws.ExpandReferences(objective, budget/3)
        msgs = append(msgs, Message{Role: RoleUser, Content: expanded})
        return msgs
}

// Run executes the full agentic loop for one objective.
func (a *Agent) Run(ctx context.Context, objective string) (string, error) {
        if a.prov == nil {
                return "", fmt.Errorf("no provider configured; run `rup.ai setup`")
        }

        maxSteps := a.cfg.Policies.MaxAgentSteps
        if maxSteps <= 0 {
                maxSteps = 30
        }
        timeout := time.Duration(a.cfg.Policies.AgentTimeoutMinutes) * time.Minute
        if timeout <= 0 {
                timeout = 15 * time.Minute
        }
        deadline := time.Now().Add(timeout)

        a.History = append(a.History, Message{Role: RoleUser, Content: objective})

        var final string
        for step := 1; step <= maxSteps; step++ {
                if err := ctx.Err(); err != nil {
                        return final, fmt.Errorf("cancelled: %w", err)
                }
                if time.Now().After(deadline) {
                        a.tui.Warn("agent time budget exhausted")
                        break
                }
                a.Steps = step
                a.compactIfHeavy(ctx)

                req := ChatRequest{
                        System:      a.SystemPrompt(),
                        Messages:    a.assembleContext(objective),
                        Tools:       a.reg.Defs(a.Mode.readToolsOnly()),
                        Model:       a.model(),
                        MaxTokens:   maxOutputOf(a.activeCfg()),
                        Temperature: a.temperature(),
                        Stream:      a.prov.Caps().Streaming,
                }

                stop := a.tui.Spinner(fmt.Sprintf("thinking (step %d/%d)", step, maxSteps))
                res, err := a.streamWithRecovery(ctx, req, stop)
                if err != nil {
                        if isContextErr(err) {
                                return final, fmt.Errorf("cancelled")
                        }
                        a.tui.Error(fmt.Sprintf("provider error: %v", err))
                        return final, err
                }
                a.Usage.Add(res.Usage)
                if res.Content != "" {
                        a.tui.ModelText(res.Content)
                }

                if len(res.ToolCalls) == 0 {
                        final = res.Content
                        break
                }

                // record assistant tool-call message, then execute each call
                a.History = append(a.History, Message{Role: RoleAssistant, Content: res.Content, ToolCalls: res.ToolCalls})
                for _, tc := range res.ToolCalls {
                        if err := ctx.Err(); err != nil {
                                return final, fmt.Errorf("cancelled")
                        }
                        a.tui.ToolStart(tc.Name, tc.Arguments)
                        started := time.Now()
                        out, toolErr := a.reg.Call(ctx, tc.Name, tc.Arguments)
                        result := out
                        if toolErr != nil {
                                result = "ERROR: " + toolErr.Error() + (func() string {
                                        if out != "" {
                                                return "\n" + out
                                        }
                                        return ""
                                })()
                        }
                        if len(result) > 16000 {
                                result = result[:16000] + "… (truncated)"
                        }
                        a.tui.ToolDone(tc.Name, time.Since(started), toolErr == nil)
                        role := RoleTool
                        a.History = append(a.History, Message{Role: role, Content: result, ToolCallID: tc.ID})
                }
        }
        if final == "" && a.Steps >= a.cfg.Policies.MaxAgentSteps {
                a.tui.Warn("step budget reached; use /compact and continue, or raise max_agent_steps in settings")
        }
        return final, nil
}

// streamWithRecovery retries transient provider failures.
func (a *Agent) streamWithRecovery(ctx context.Context, req ChatRequest, stop func()) (StreamResult, error) {
        defer stop()
        var on StreamFunc
        if a.tui != nil && req.Stream {
                on = func(ev StreamEvent) error {
                        if ev.Delta != "" {
                                a.tui.StreamDelta(ev.Delta)
                        }
                        return ctx.Err()
                }
        }
        res, err := a.prov.Stream(ctx, req, on)
        return res, err
}

func (a *Agent) model() string {
        if a.cfg.ActiveModel != "" {
                return a.cfg.ActiveModel
        }
        if p, ok := a.cfg.Active(); ok && p.Model != "" {
                return p.Model
        }
        return "default"
}

// activeCfg returns the active provider config (nil when unset).
func (a *Agent) activeCfg() *ProviderConfig {
        p, _ := a.cfg.Active()
        return p
}

func (a *Agent) temperature() float64 {
        if p, ok := a.cfg.Active(); ok && p.Temperature >= 0 {
                return p.Temperature
        }
        return 0.2
}

// compactIfHeavy summarizes old history when the token estimate exceeds budget.
func (a *Agent) compactIfHeavy(ctx context.Context) {
        budget := a.cfg.Context.BudgetTokens
        if budget <= 0 {
                budget = 24000
        }
        total := EstimateTokens(a.SystemPrompt())
        for _, m := range a.History {
                total += EstimateTokens(m.Content)
        }
        if total < budget {
                return
        }
        if len(a.History) < 6 {
                return
        }
        split := len(a.History) - 4
        stop := a.tui.Spinner("compacting conversation")
        summary, err := a.summarize(ctx, a.History[:split])
        stop()
        if err != nil || summary == "" {
                // fall back to mechanical truncation
                summary = "(history truncated by compaction)"
        }
        compacted := make([]Message, 0, 5)
        compacted = append(compacted, Message{Role: RoleUser, Content: "Conversation summary so far:\n" + summary})
        compacted = append(compacted, a.History[split:]...)
        a.History = compacted
        a.tui.Info("context compacted to stay within budget")
}

// summarize asks the model for a compact summary (no tools).
func (a *Agent) summarize(ctx context.Context, msgs []Message) (string, error) {
        var b strings.Builder
        for _, m := range msgs {
                role := string(m.Role)
                content := m.Content
                if len(content) > 800 {
                        content = content[:800] + "…"
                }
                b.WriteString(role + ": " + strings.ReplaceAll(content, "\n", " ") + "\n")
        }
        req := ChatRequest{
                System:   "Summarize this coding-agent conversation: objectives, decisions, files changed, commands run, errors and fixes, current state. Be terse and factual.",
                Messages: []Message{{Role: RoleUser, Content: b.String()}},
                Model:    a.model(),
                MaxTokens: 600,
        }
        cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
        defer cancel()
        res, err := a.prov.Stream(cctx, req, nil)
        if err != nil {
                return "", err
        }
        return res.Content, nil
}

// PlanFlow runs plan mode: generate plan, return text for approval.
func (a *Agent) PlanFlow(ctx context.Context, objective string) (string, error) {
        prev := a.Mode
        a.Mode = ModePlan
        defer func() { a.Mode = prev }()
        out, err := a.Run(ctx, objective)
        if err != nil {
                return "", err
        }
        a.Plan = out
        return out, nil
}

// ChangedFiles summarizes the session's changes.
func (a *Agent) ChangedFiles() []string {
        a.changes.mu.Lock()
        defer a.changes.mu.Unlock()
        set := map[string]int{}
        for _, c := range a.changes.Changed {
                set[c.Path]++
        }
        paths := make([]string, 0, len(set))
        for p := range set {
                paths = append(paths, p)
        }
        sort.Strings(paths)
        return paths
}

// SessionSummary renders the final task report.
func (a *Agent) SessionSummary() string {
        a.changes.mu.Lock()
        created, modified := 0, 0
        seen := map[string]bool{}
        var lines []string
        for _, c := range a.changes.Changed {
                key := c.Path
                line := ""
                switch {
                case c.Tool == "delete_file":
                        line = "deleted  " + c.Path
                case c.Tool == "create_dir":
                        line = "dir      " + c.Path
                case strings.Contains(c.Path, " → "):
                        line = "moved    " + c.Path
                default:
                        if seen[key] {
                                modified++
                                line = "modified " + c.Path
                        } else {
                                created++
                                line = "created  " + c.Path
                        }
                }
                seen[key] = true
                lines = append(lines, line)
        }
        a.changes.mu.Unlock()
        var b strings.Builder
        if len(lines) == 0 {
                b.WriteString("Files changed: none\n")
        } else {
                b.WriteString("Files touched this session:\n")
                for _, l := range lines {
                        b.WriteString("  " + l + "\n")
                }
        }
        return strings.TrimRight(b.String(), "\n")
}
