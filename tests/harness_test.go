package tests

// harness_test.go — test infrastructure: builds the rup.ai binary once,
// provides isolated HOME/config/data dirs, and implements local
// OpenAI/Anthropic protocol test servers (no external services, no mocks of
// rup.ai internals — the real HTTP client code paths are exercised).

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "net/http"
        "net/http/httptest"
        "os"
        "os/exec"
        "path/filepath"
        "runtime"
        "strings"
        "sync"
        "testing"
        "time"
)

var binaryPath string

func TestMain(m *testing.M) {
        tmp, err := os.MkdirTemp("", "rupai-bin-*")
        if err != nil {
                fmt.Fprintln(os.Stderr, "testmain:", err)
                os.Exit(1)
        }
        defer os.RemoveAll(tmp)

        if goPath, lookErr := exec.LookPath("go"); lookErr == nil {
                ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
                defer cancel()
                cmd := exec.CommandContext(ctx, goPath, "build", "-o", filepath.Join(tmp, "rupai"), "./backend")
                cmd.Dir = projectRoot()
                out, err := cmd.CombinedOutput()
                if err != nil {
                        fmt.Fprintf(os.Stderr, "failed to build rup.ai binary:\n%s\n", out)
                        os.Exit(1)
                }
                binaryPath = filepath.Join(tmp, "rupai")
        } else {
                fmt.Fprintln(os.Stderr, "go not on PATH; binary-dependent tests will be skipped")
        }

        code := m.Run()
        os.Exit(code)
}

// projectRoot returns the repository root (tests/ sits directly under it).
func projectRoot() string {
        _, thisFile, _, ok := runtime.Caller(0)
        if !ok {
                panic("cannot determine test dir")
        }
        return filepath.Clean(filepath.Join(filepath.Dir(thisFile), ".."))
}

// isolatedEnv returns env vars pointing config/data at a fresh temp dir.
func isolatedEnv(t *testing.T) []string {
        t.Helper()
        home := t.TempDir()
        return []string{
                "HOME=" + home,
                "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
                "APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
                "RUPAI_DATA_DIR=" + filepath.Join(home, "data"),
                "RUPAI_NO_ANIM=1",
                "NO_COLOR=1",
        }
}

// runCLI runs the built binary with env, working dir, stdin; returns stdout,
// stderr and exit code.
func runCLI(t *testing.T, env []string, dir string, stdin string, args ...string) (string, string, int) {
        t.Helper()
        if binaryPath == "" {
                t.Skip("binary not built (go missing)")
        }
        env = dedupEnv(env)
        cmd := exec.Command(binaryPath, args...)
        cmd.Env = append(os.Environ(), env...)
        cmd.Dir = dir
        cmd.Stdin = strings.NewReader(stdin)
        var outBuf, errBuf strings.Builder
        cmd.Stdout = &outBuf
        cmd.Stderr = &errBuf
        err := cmd.Run()
        exit := 0
        if ee, ok := err.(*exec.ExitError); ok {
                exit = ee.ExitCode()
        } else if err != nil {
                t.Fatalf("failed to run binary: %v", err)
        }
        return outBuf.String(), errBuf.String(), exit
}

// dedupEnv keeps the LAST occurrence of each VAR=... (test overrides win).
func dedupEnv(env []string) []string {
        seen := map[string]int{}
        out := make([]string, 0, len(env))
        for _, kv := range env {
                key := kv
                if i := strings.Index(kv, "="); i >= 0 {
                        key = kv[:i]
                }
                if idx, ok := seen[key]; ok {
                        out[idx] = kv
                } else {
                        seen[key] = len(out)
                        out = append(out, kv)
                }
        }
        return out
}

// --- scripted OpenAI-compatible test server --------------------------------

type toolCallOut struct {
        Index    int    `json:"index"`
        ID       string `json:"id,omitempty"`
        Type     string `json:"type,omitempty"`
        Function struct {
                Name      string `json:"name,omitempty"`
                Arguments string `json:"arguments,omitempty"`
        } `json:"function"`
}

type Turn struct {
        Content   string
        ToolCalls []toolCallOut
}

// CapturedRequest stores the last parsed chat completion request.
type CapturedRequest struct {
        mu       sync.Mutex
        Body     []byte
        Model    string
        Stream   bool
        Auth     string
        Headers  http.Header
        Messages []struct {
                Role      string `json:"role"`
                Content   string `json:"content"`
                ToolCallID string `json:"tool_call_id,omitempty"`
        } `json:"messages"`
        System string   `json:"-"`
        Tools  []struct {
                Function struct {
                        Name string `json:"name"`
                } `json:"function"`
        } `json:"tools"`
}

func (c *CapturedRequest) Last() []byte {
        c.mu.Lock()
        defer c.mu.Unlock()
        return c.Body
}

// newOpenAITestServer serves /v1/chat/completions (stream + non-stream) and
// /v1/models with the given scripted turns (last turn repeats).
func newOpenAITestServer(t *testing.T, turns []Turn, captured *CapturedRequest) *httptest.Server {
        t.Helper()
        idx := 0
        var mu sync.Mutex
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                switch {
                case strings.HasSuffix(r.URL.Path, "/models"):
                        w.Header().Set("Content-Type", "application/json")
                        _ = json.NewEncoder(w).Encode(map[string]any{
                                "data": []map[string]string{{"id": "test-model-1"}, {"id": "test-model-2"}},
                        })
                        return
                case strings.HasSuffix(r.URL.Path, "/chat/completions"):
                default:
                        http.NotFound(w, r)
                        return
                }

                raw, _ := readAll(r.Body)
                captured.mu.Lock()
                captured.Body = raw
                captured.Headers = r.Header.Clone()
                captured.Auth = r.Header.Get("Authorization")
                captured.Model = ""
                captured.Stream = false
                var req struct {
                        Model    string `json:"model"`
                        Stream   bool   `json:"stream"`
                        Messages []struct {
                                Role      string `json:"role"`
                                Content   string `json:"content"`
                                ToolCallID string `json:"tool_call_id,omitempty"`
                        } `json:"messages"`
                        Tools []struct {
                                Function struct {
                                        Name string `json:"name"`
                                } `json:"function"`
                        } `json:"tools"`
                }
                if json.Unmarshal(raw, &req) == nil {
                        captured.Model = req.Model
                        captured.Stream = req.Stream
                        captured.Messages = req.Messages
                        captured.Tools = req.Tools
                }
                captured.mu.Unlock()

                mu.Lock()
                turn := turns[len(turns)-1]
                if idx < len(turns) {
                        turn = turns[idx]
                }
                idx++
                mu.Unlock()

                w.Header().Set("Content-Type", "application/json")
                if captured.isStream() {
                        writeOpenAISSE(w, turn)
                        return
                }
                msg := map[string]any{
                        "role":    "assistant",
                        "content": turn.Content,
                }
                if len(turn.ToolCalls) > 0 {
                        msg["content"] = turn.Content
                        msg["tool_calls"] = turn.ToolCalls
                }
                _ = json.NewEncoder(w).Encode(map[string]any{
                        "choices":  []map[string]any{{"index": 0, "message": msg, "finish_reason": "stop"}},
                        "usage":    map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
                })
        }))
        t.Cleanup(srv.Close)
        return srv
}

// isStream reports whether the last request asked for streaming.
func (c *CapturedRequest) isStream() bool {
        c.mu.Lock()
        defer c.mu.Unlock()
        return c.Stream
}

func writeOpenAISSE(w http.ResponseWriter, turn Turn) {
        flusher := w.(http.Flusher)
        emit := func(payload string) {
                fmt.Fprintf(w, "data: %s\n\n", payload)
                flusher.Flush()
        }
        emit(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
        if turn.Content != "" {
                payload, _ := json.Marshal(map[string]any{
                        "choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": turn.Content}}},
                })
                emit(string(payload))
        }
        for _, tc := range turn.ToolCalls {
                payload, _ := json.Marshal(map[string]any{
                        "choices": []map[string]any{{"index": 0, "delta": map[string]any{
                                "tool_calls": []toolCallOut{tc},
                        }}},
                })
                emit(string(payload))
        }
        final, _ := json.Marshal(map[string]any{
                "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
                "usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
        })
        emit(string(final))
        emit("[DONE]")
}

func readAll(r io.Reader) ([]byte, error) {
        const max = 32 << 20
        return io.ReadAll(io.LimitReader(r, max))
}

// newAnthropicTestServer serves /v1/messages and /v1/models.
func newAnthropicTestServer(t *testing.T, turns []Turn, captured *CapturedRequest) *httptest.Server {
        t.Helper()
        idx := 0
        var mu sync.Mutex
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                switch {
                case strings.HasSuffix(r.URL.Path, "/v1/models"):
                        w.Header().Set("Content-Type", "application/json")
                        _ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "claude-test"}}})
                        return
                case strings.HasSuffix(r.URL.Path, "/v1/messages"):
                default:
                        http.NotFound(w, r)
                        return
                }
                raw, _ := readAll(r.Body)
                captured.mu.Lock()
                captured.Body = raw
                captured.Headers = r.Header.Clone()
                var req struct {
                        Model    string `json:"model"`
                        Stream   bool   `json:"stream"`
                        System   string `json:"system"`
                        Messages []struct {
                                Role    string `json:"role"`
                                Content any    `json:"content"`
                        } `json:"messages"`
                }
                if json.Unmarshal(raw, &req) == nil {
                        captured.Model = req.Model
                        captured.Stream = req.Stream
                        captured.System = req.System
                }
                captured.mu.Unlock()

                mu.Lock()
                turn := turns[len(turns)-1]
                if idx < len(turns) {
                        turn = turns[idx]
                }
                idx++
                mu.Unlock()

                w.Header().Set("Content-Type", "text/event-stream")
                writeAnthropicSSE(w, turn)
        }))
        t.Cleanup(srv.Close)
        return srv
}

func writeAnthropicSSE(w http.ResponseWriter, turn Turn) {
        flusher := w.(http.Flusher)
        event := func(name, payload string) {
                fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
                flusher.Flush()
        }
        event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":13}}}`)
        if len(turn.ToolCalls) > 0 {
                if turn.Content != "" {
                        b, _ := json.Marshal(turn.Content)
                        event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`))
                        event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, b))
                        event("content_block_stop", `{"type":"content_block_stop","index":0}`)
                }
                for i, tc := range turn.ToolCalls {
                        event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, i+1, tc.ID, tc.Function.Name))
                        event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, i+1, mustJSON(tc.Function.Arguments)))
                        event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i+1))
                }
        } else {
                b, _ := json.Marshal(turn.Content)
                event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
                event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, b))
                event("content_block_stop", `{"type":"content_block_stop","index":0}`)
        }
        event("message_delta", `{"type":"message_delta","usage":{"output_tokens":21}}`)
        event("message_stop", `{"type":"message_stop"}`)
}

func mustJSON(s string) string {
        b, _ := json.Marshal(s)
        return string(b)
}

// newHangingServer never answers — used to test cancellation. The handler
// blocks until the client goes away OR the test releases it, so cleanup
// never deadlocks regardless of how the transport behaves on abort.
func newHangingServer(t *testing.T) *testServerHandle {
        t.Helper()
        release := make(chan struct{})
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                select {
                case <-release:
                case <-r.Context().Done():
                }
        }))
        t.Cleanup(func() {
                close(release)
                srv.Close()
        })
        return &testServerHandle{URL: srv.URL}
}

// mustListen binds an ephemeral loopback port.
func mustListen(t *testing.T) net.Listener {
        t.Helper()
        ln, err := net.Listen("tcp", "127.0.0.1:0")
        if err != nil {
                t.Fatal(err)
        }
        return ln
}

func portOf(ln net.Listener) string {
        return fmt.Sprintf(":%d", ln.Addr().(*net.TCPAddr).Port)
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
        return context.WithTimeout(context.Background(), d)
}
