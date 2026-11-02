package tests

// agent_e2e_test.go — end-to-end agent tests against local protocol test
// servers: real CLI startup, real provider HTTP calls, real tool execution
// against a temporary workspace.

import (
        "encoding/json"
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "testing"
        "time"
)

const (
        testAPIKey  = "sk-test-rupai-key-000000000000"
        testModelID = "test-model-1"
)

// envForProvider builds env vars pointing rup.ai at a test server.
// Approval defaults to permissive so scripted tool flows run unattended;
// individual tests override it to exercise standard/restricted policies.
func envForProvider(env []string, baseURL, kind string) []string {
        return append(env,
                "RUPAI_PROVIDER=testprov",
                "RUPAI_KIND="+kind,
                "RUPAI_BASE_URL="+baseURL,
                "RUPAI_MODEL="+testModelID,
                "RUPAI_API_KEY="+testAPIKey,
                "RUPAI_MAX_STEPS=6",
                "RUPAI_APPROVAL=permissive",
        )
}

func toolCallJSON(t *testing.T, id, name string, args map[string]any) toolCallOut {
        t.Helper()
        raw, err := json.Marshal(args)
        if err != nil {
                t.Fatal(err)
        }
        var tc toolCallOut
        tc.Index = 0
        tc.ID = id
        tc.Type = "function"
        tc.Function.Name = name
        tc.Function.Arguments = string(raw)
        return tc
}

func TestCLIHelpAndVersion(t *testing.T) {
        env := isolatedEnv(t)
        out, _, code := runCLI(t, env, t.TempDir(), "", "version")
        if code != 0 || !strings.Contains(out, "rup.ai") {
                t.Fatalf("version: code=%d out=%q", code, out)
        }
        out, _, code = runCLI(t, env, t.TempDir(), "", "--help")
        if code != 0 || !strings.Contains(out, "setup") || !strings.Contains(out, "doctor") {
                t.Fatalf("help: code=%d out=%q", code, out)
        }
}

func TestWizardSkipFlow(t *testing.T) {
        env := isolatedEnv(t)
        out, _, code := runCLI(t, env, t.TempDir(), "9\n")
        if code != 0 {
                t.Fatalf("wizard skip: code=%d", code)
        }
        if !strings.Contains(out, "Welcome to rup.ai") || !strings.Contains(out, "Configure later") {
                t.Fatalf("wizard output missing expected text:\n%s", out)
        }
        if !strings.Contains(out, "rup.ai setup") {
                t.Fatalf("skip flow must explain how to configure later:\n%s", out)
        }
}

func TestOpenAIProtocolEndToEnd(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        // turn 1: model asks to create a file; turn 2: model summarizes
        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "call_1", "write_file", map[string]any{
                                "path": "hello.txt", "content": "created by rup.ai e2e test",
                        }),
                }},
                {Content: "E2E COMPLETE: hello.txt written."},
        }
        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")

        out, stderr, code := runCLI(t, env, ws, "", "Create hello.txt with fixed content and finish.")
        if code != 0 {
                t.Fatalf("exit=%d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
        }

        // the file really exists — real filesystem, not fabricated output
        data, err := os.ReadFile(filepath.Join(ws, "hello.txt"))
        if err != nil {
                t.Fatalf("agent did not create hello.txt: %v", err)
        }
        if string(data) != "created by rup.ai e2e test" {
                t.Fatalf("unexpected file content: %q", string(data))
        }
        if !strings.Contains(out, "created") {
                t.Fatalf("summary should list created files:\n%s", out)
        }

        // protocol conformance: auth header, model, tools, tool result round-trip
        captured.mu.Lock()
        defer captured.mu.Unlock()
        if captured.Auth != "Bearer "+testAPIKey {
                t.Fatalf("missing bearer auth: %q", captured.Auth)
        }
        if captured.Model != testModelID {
                t.Fatalf("wrong model sent: %q", captured.Model)
        }
        if len(captured.Tools) == 0 {
                t.Fatalf("tools not advertised to the model")
        }
        sawToolResult := false
        for _, m := range captured.Messages {
                if m.Role == "tool" && strings.Contains(m.Content, "hello.txt") {
                        sawToolResult = true
                }
        }
        if !sawToolResult {
                t.Fatalf("tool result was not fed back to the model: %+v", captured.Messages)
        }
}

func TestAnthropicProtocolEndToEnd(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "toolu_1", "edit_file", map[string]any{
                                "path": "app.md", "old_text": "alpha", "new_text": "beta",
                        }),
                }},
                {Content: "ANTHROPIC E2E: edited app.md."},
        }
        srv := newAnthropicTestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "anthropic")

        seed := filepath.Join(ws, "app.md")
        if err := os.WriteFile(seed, []byte("alpha document\n"), 0o644); err != nil {
                t.Fatal(err)
        }

        out, stderr, code := runCLI(t, env, ws, "", "Replace alpha with beta in app.md.")
        if code != 0 {
                t.Fatalf("exit=%d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
        }

        data, err := os.ReadFile(seed)
        if err != nil || string(data) != "beta document\n" {
                t.Fatalf("edit_file did not apply: %v %q", err, string(data))
        }

        captured.mu.Lock()
        defer captured.mu.Unlock()
        if got := captured.Headers.Get("x-api-key"); got != testAPIKey {
                t.Fatalf("anthropic auth header wrong: %q", got)
        }
        if got := captured.Headers.Get("anthropic-version"); got == "" {
                t.Fatalf("anthropic-version header missing")
        }
        if !strings.Contains(captured.System, "rup.ai") {
                t.Fatalf("system prompt not sent via the anthropic system field")
        }
        // tool_result must be returned as a user message with tool_result blocks
        body := string(captured.Body)
        if !strings.Contains(body, "tool_result") || !strings.Contains(body, "toolu_1") {
                t.Fatalf("tool result not marshaled as anthropic tool_result:\n%s", body)
        }
}

func TestWorkspaceJail(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}
        outside := filepath.Join(filepath.Dir(ws), "jail-escape-probe.txt")
        _ = os.Remove(outside)

        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "call_9", "write_file", map[string]any{
                                "path": "../../jail-escape-probe.txt", "content": "should not exist",
                        }),
                }},
                {Content: "done (refused)."},
        }
        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")

        out, _, code := runCLI(t, env, ws, "", "try to escape the workspace")
        if code != 0 {
                t.Fatalf("exit=%d out=%s", code, out)
        }
        if _, err := os.Stat(outside); err == nil {
                t.Fatalf("agent escaped the workspace: %s exists", outside)
        }
        sawRefusal := false
        captured.mu.Lock()
        for _, m := range captured.Messages {
                if m.Role == "tool" && strings.Contains(m.Content, "refused") {
                        sawRefusal = true
                }
        }
        captured.mu.Unlock()
        if !sawRefusal {
                t.Fatalf("the model must receive the refusal reason")
        }
}

func TestCommandSecurityPolicy(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "c1", "run_command", map[string]any{"command": "echo rupai-e2e-marker"}),
                }},
                {Content: "COMMAND TEST DONE."},
        }
        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")
        env = append(env, "RUPAI_APPROVAL=standard")

        out, _, code := runCLI(t, env, ws, "", "run the echo command")
        if code != 0 {
                t.Fatalf("exit=%d out=%s", code, out)
        }
        if !strings.Contains(out, "rupai-e2e-marker") {
                t.Fatalf("safe command should auto-run and its output be returned:\n%s", out)
        }

        // catastrophic command must be blocked outright
        captured2 := &CapturedRequest{}
        turns2 := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "c2", "run_command", map[string]any{"command": "rm -rf /"}),
                }},
                {Content: "blocked test done."},
        }
        srv2 := newOpenAITestServer(t, turns2, captured2)
        env2 := envForProvider(isolatedEnv(t), srv2.URL, "openai")
        out2, _, code2 := runCLI(t, env2, t.TempDir(), "", "try rm -rf /")
        if code2 != 0 {
                t.Fatalf("blocked test exit=%d out=%s", code2, out2)
        }
        if strings.Contains(out2, "blocked by security policy") == false &&
                !containsToolError(t, captured2, "blocked by security policy") {
                t.Fatalf("rm -rf / must be blocked, got:\n%s", out2)
        }
}

func containsToolError(t *testing.T, captured *CapturedRequest, needle string) bool {
        t.Helper()
        captured.mu.Lock()
        defer captured.mu.Unlock()
        for _, m := range captured.Messages {
                if m.Role == "tool" && strings.Contains(m.Content, needle) {
                        return true
                }
        }
        return false
}

func TestApprovalPromptDeniesCommand(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        // a non-whitelisted command under `restricted` policy requires approval
        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "c3", "run_command", map[string]any{"command": "touch approval-probe.txt"}),
                }},
                {Content: "approval flow done."},
        }
        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")
        env = append(env, "RUPAI_APPROVAL=restricted")

        _, _, code := runCLI(t, env, ws, "n\n", "run the touch command")
        if code != 0 {
                t.Fatalf("exit code %d", code)
        }
        if _, err := os.Stat(filepath.Join(ws, "approval-probe.txt")); err == nil {
                t.Fatalf("denied command must not execute")
        }
        if !containsToolError(t, captured, "user declined") {
                t.Fatalf("denial must be fed back to the model")
        }
}

func TestGitIntegrationAndContextAssembly(t *testing.T) {
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        // seed a project: README (guidance), .gitignore, node_modules noise
        if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# Sample Project\n\nUse tab indentation. API base is /api/v1.\n"), 0o644); err != nil {
                t.Fatal(err)
        }
        if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("node_modules/\nsecrets.txt\n"), 0o644); err != nil {
                t.Fatal(err)
        }
        if err := os.MkdirAll(filepath.Join(ws, "node_modules", "leftpad"), 0o755); err != nil {
                t.Fatal(err)
        }
        if err := os.WriteFile(filepath.Join(ws, "node_modules", "leftpad", "index.js"), []byte("module.exports = p => p + 1;\n"), 0o644); err != nil {
                t.Fatal(err)
        }
        // init a real git repo
        if err := runInDir(ws, "git", "init", "-q"); err != nil {
                t.Skipf("git unavailable: %v", err)
        }

        turns := []Turn{
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "g1", "git_status", map[string]any{}),
                }},
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "g2", "run_command", map[string]any{"command": "git log --oneline -1 2>/dev/null || echo no-commits"}),
                }},
                {Content: "GIT TEST DONE."},
        }
        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")

        out, _, code := runCLI(t, env, ws, "", "inspect this repository")
        if code != 0 {
                t.Fatalf("exit=%d out=%s", code, out)
        }

        captured.mu.Lock()
        systemText := ""
        for _, m := range captured.Messages {
                if m.Role == "system" || m.Content == "" {
                        // openai wire: system is a message with role system
                }
                if m.Role == "system" {
                        systemText = m.Content
                }
        }
        // non-stream requests put system as first message
        if systemText == "" && len(captured.Messages) > 0 && captured.Messages[0].Role == "system" {
                systemText = captured.Messages[0].Content
        }
        captured.mu.Unlock()

        if systemText == "" {
                // streaming wire: system is sent as a normal message; fall back to first user content
                captured.mu.Lock()
                if len(captured.Messages) > 0 {
                        systemText = captured.Messages[0].Content
                }
                captured.mu.Unlock()
        }
        if strings.Contains(systemText, "leftpad") || strings.Contains(systemText, "node_modules/leftpad") {
                t.Fatalf("context assembly must honor .gitignore (node_modules leaked):\n%s", systemText[:min2(800, len(systemText))])
        }
        if !strings.Contains(systemText, "Sample Project") {
                t.Fatalf("README guidance should be included in context")
        }
        // the git_status tool result (with the branch) must be fed back to the model
        sawBranch := false
        captured.mu.Lock()
        for _, m := range captured.Messages {
                if m.Role == "tool" && (strings.Contains(m.Content, "main") || strings.Contains(m.Content, "master")) {
                        sawBranch = true
                }
        }
        captured.mu.Unlock()
        if !sawBranch {
                t.Fatalf("git_status result with the branch should reach the model")
        }
}

func min2(a, b int) int {
        if a < b {
                return a
        }
        return b
}

func runInDir(dir string, name string, args ...string) error {
        cmd := exec.Command(name, args...)
        cmd.Dir = dir
        cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@example.com",
                "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@example.com")
        return cmd.Run()
}

func TestCancellation(t *testing.T) {
        if binaryPath == "" {
                t.Skip("binary not built")
        }
        ws := t.TempDir()
        env := isolatedEnv(t)

        // a server that never answers in time
        srvHold := newHangingServer(t)
        env = envForProvider(env, srvHold.URL, "openai")
        env = append(env, "RUPAI_MAX_STEPS=3")

        cmd := exec.Command(binaryPath, "hang please")
        cmd.Env = append(os.Environ(), env...)
        cmd.Dir = ws
        var out strings.Builder
        cmd.Stdout = &out
        cmd.Stderr = &out
        if err := cmd.Start(); err != nil {
                t.Fatal(err)
        }
        done := make(chan error, 1)
        go func() { done <- cmd.Wait() }()

        time.Sleep(2 * time.Second)
        if err := cmd.Process.Signal(os.Interrupt); err != nil {
                _ = cmd.Process.Kill()
                t.Fatalf("cannot signal: %v", err)
        }
        select {
        case <-done:
                // expected: process exits after cancellation
        case <-time.After(20 * time.Second):
                _ = cmd.Process.Kill()
                t.Fatalf("process did not exit within 20s of Ctrl+C")
        }
}

func TestWindowsCrossCompile(t *testing.T) {
        if _, err := exec.LookPath("go"); err != nil {
                t.Skip("go missing")
        }
        out := filepath.Join(t.TempDir(), "rupai.exe")
        cmd := exec.Command("go", "build", "-o", out, "./backend")
        cmd.Dir = projectRoot()
        cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
        if o, err := cmd.CombinedOutput(); err != nil {
                t.Fatalf("windows cross-compile failed: %v\n%s", err, o)
        }
        if st, err := os.Stat(out); err != nil || st.Size() == 0 {
                t.Fatalf("windows binary missing/empty")
        }
}
