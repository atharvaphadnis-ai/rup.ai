package tests

// acceptance_test.go — the end-to-end acceptance scenario from the product
// spec (section 24), driven by a scripted local provider:
// inspect → create files → run REAL tests → read the real failure →
// fix → re-run → pass → report. Uses python3 (available on dev machines
// and CI); skipped when python3 is absent.

import (
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "testing"
)

func TestFullStackAcceptanceLoop(t *testing.T) {
        if _, err := exec.LookPath("python3"); err != nil {
                t.Skip("python3 not available")
        }
        ws := t.TempDir()
        env := isolatedEnv(t)
        captured := &CapturedRequest{}

        appPy := `def add(a, b):
    return a - b
`
        testPy := `from app import add

def test_add():
    assert add(2, 3) == 5
`

        turns := []Turn{
                // 1) inspect the workspace
                {ToolCalls: []toolCallOut{toolCallJSON(t, "a1", "detect_project", map[string]any{})}},
                // 2) create the application + a failing test
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "a2", "write_file", map[string]any{"path": "app.py", "content": appPy}),
                        toolCallJSON(t, "a3", "write_file", map[string]any{"path": "test_app.py", "content": testPy}),
                }},
                // 3) run the tests — they will FAIL (bug: subtraction)
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "a4", "run_command", map[string]any{"command": "python3 -m pytest -q"}),
                }},
                // 4) read the error, fix the bug
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "a5", "edit_file", map[string]any{
                                "path": "app.py", "old_text": "return a - b", "new_text": "return a + b"}),
                }},
                // 5) re-run the tests — PASS
                {ToolCalls: []toolCallOut{
                        toolCallJSON(t, "a6", "run_command", map[string]any{"command": "python3 -m pytest -q"}),
                }},
                // 6) summarize
                {Content: "ACCEPTANCE COMPLETE. Fixed app.py, tests pass."},
        }

        srv := newOpenAITestServer(t, turns, captured)
        env = envForProvider(env, srv.URL, "openai")

        out, stderr, code := runCLI(t, env, ws, "", "Build a tiny python lib with a test, run the tests, fix failures.")
        if code != 0 {
                t.Fatalf("exit=%d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
        }

        // files really created
        data, err := os.ReadFile(filepath.Join(ws, "app.py"))
        if err != nil {
                t.Fatalf("app.py missing: %v", err)
        }
        if !strings.Contains(string(data), "return a + b") {
                t.Fatalf("the fix was not applied:\n%s", string(data))
        }
        if _, err := os.ReadFile(filepath.Join(ws, "test_app.py")); err != nil {
                t.Fatalf("test_app.py missing: %v", err)
        }

        // the failing run and the passing run both really happened:
        // pytest's failing run prints "= 1 failed", the passing one "1 passed"
        captured.mu.Lock()
        sawFailed := false
        sawPassed := false
        for _, m := range captured.Messages {
                if m.Role != "tool" {
                        continue
                }
                if strings.Contains(m.Content, "failed") && strings.Contains(m.Content, "test_add") {
                        sawFailed = true
                }
                if strings.Contains(m.Content, "passed") {
                        sawPassed = true
                }
        }
        captured.mu.Unlock()
        if !sawFailed {
                t.Fatalf("the real failing test output was not fed back to the model")
        }
        if !sawPassed {
                t.Fatalf("the real passing test output was not fed back to the model")
        }

        // the transcript reports the fix honestly
        if !strings.Contains(out, "modified") && !strings.Contains(out, "edited") {
                t.Fatalf("summary should show modified files:\n%s", out)
        }
}
