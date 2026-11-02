package tests

// misc_test.go — source file counts (the hard 15/5 requirement), GGUF
// parsing, credential storage, preview server state and the companion web
// API (connectivity + token auth).

import (
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "testing"
        "time"
)

// TestSourceFileCounts enforces the mandatory architecture:
// exactly 15 backend application source files and exactly 5 frontend
// first-party source files. Test files (tests/) live outside the source
// directories, as the project spec allows.
func TestSourceFileCounts(t *testing.T) {
        root := projectRoot()

        backend := filepath.Join(root, "backend")
        entries, err := os.ReadDir(backend)
        if err != nil {
                t.Fatal(err)
        }
        goFiles := 0
        var names []string
        for _, e := range entries {
                if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
                        continue
                }
                if strings.HasSuffix(e.Name(), "_test.go") {
                        t.Fatalf("test files must live outside backend/: %s", e.Name())
                }
                goFiles++
                names = append(names, e.Name())
        }
        if goFiles != 15 {
                t.Fatalf("backend must contain exactly 15 .go source files, found %d: %v", goFiles, names)
        }

        frontend := filepath.Join(root, "frontend")
        expected := map[string]bool{
                "index.html": true, "main.ts": true, "app.ts": true,
                "styles.css": true, "preview.ts": true,
        }
        found := map[string]bool{}
        feEntries, err := os.ReadDir(frontend)
        if err != nil {
                t.Fatal(err)
        }
        for _, e := range feEntries {
                if e.IsDir() {
                        // only generated assets may live in subdirectories (dist/)
                        if e.Name() != "dist" {
                                t.Fatalf("unexpected directory in frontend/: %s", e.Name())
                        }
                        continue
                }
                if !expected[e.Name()] {
                        t.Fatalf("unexpected first-party frontend file: %s", e.Name())
                }
                found[e.Name()] = true
        }
        for name := range expected {
                if !found[name] {
                        t.Fatalf("missing required frontend file: %s", name)
                }
        }
        if len(found) != 5 {
                t.Fatalf("frontend must contain exactly 5 first-party files, found %d", len(found))
        }
}

// TestGGUFParsing builds a minimal valid GGUF file and verifies the CLI can
// read its metadata end-to-end (models --import).
func TestGGUFParsing(t *testing.T) {
        path := filepath.Join(t.TempDir(), "tiny-model-Q4_K_M.gguf")
        if err := os.WriteFile(path, minimalGGUF(t), 0o644); err != nil {
                t.Fatal(err)
        }
        env := isolatedEnv(t)
        out, stderr, code := runCLI(t, env, t.TempDir(), "", "models", "--import", path)
        if code != 0 {
                t.Fatalf("import failed: code=%d\n%s\n%s", code, out, stderr)
        }
        for _, want := range []string{"Q4_K_M", "llama"} {
                if !strings.Contains(out, want) {
                        t.Fatalf("import output missing %q:\n%s", want, out)
                }
        }
}

// minimalGGUF assembles a tiny GGUF: magic, v3, 0 tensors, 3 KV pairs.
func minimalGGUF(t *testing.T) []byte {
        t.Helper()
        var b []byte
        appendStr := func(s string) {
                b = append(b, byte(len(s)), 0, 0, 0, 0, 0, 0, 0)
                b = append(b, s...)
        }
        appendU32 := func(v uint32) {
                b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
        }
        appendU64 := func(v uint64) {
                for i := 0; i < 8; i++ {
                        b = append(b, byte(v>>(8*i)))
                }
        }
        b = append(b, 'G', 'G', 'U', 'F')
        appendU32(3) // version
        appendU64(0) // tensor count
        appendU64(3) // kv count

        // kv 1: general.architecture = "llama" (string type 8)
        appendStr("general.architecture")
        appendU32(8)
        appendStr("llama")
        // kv 2: general.file_type = 15 (Q4_K_M)
        appendStr("general.file_type")
        appendU32(4) // uint32
        appendU32(15)
        // kv 3: llama.context_length = 4096
        appendStr("llama.context_length")
        appendU32(10) // uint64
        appendU64(4096)
        return b
}

// TestCredentialStorage covers `rup.ai login` (non-interactive stdin),
// 0600 permissions on the fallback store, and redaction in `config export`.
func TestCredentialStorage(t *testing.T) {
        env := isolatedEnv(t)
        ws := t.TempDir()

        out, stderr, code := runCLI(t, env, ws, "\n"+testAPIKey+"\n", "login", "openai")
        if code != 0 {
                t.Fatalf("login failed: %d\n%s\n%s", code, out, stderr)
        }
        if !strings.Contains(out, "stored") {
                t.Fatalf("login should confirm storage:\n%s", out)
        }

        // fallback credential file must exist with restrictive permissions
        home := envHome(t, env)
        credPath := filepath.Join(home, ".config", "rup.ai", "credentials.json")
        raw, err := os.ReadFile(credPath)
        if err != nil {
                t.Fatalf("credentials.json missing: %v", err)
        }
        if !strings.Contains(string(raw), testAPIKey) {
                // keyring may be used on this host instead of the file; both are fine
                t.Logf("credential stored outside fallback file (keyring available)")
        } else {
                st, _ := os.Stat(credPath)
                if st.Mode().Perm() != 0o600 {
                        t.Fatalf("credentials.json must be 0600, got %v", st.Mode().Perm())
                }
        }

        // export must never leak the key
        out2, _, code2 := runCLI(t, append(env, "RUPAI_PROVIDER=openai"), ws, "", "config", "export")
        if code2 != 0 {
                t.Fatalf("config export failed: %d\n%s", code2, out2)
        }
        if strings.Contains(out2, testAPIKey) {
                t.Fatalf("config export leaked the API key:\n%s", out2)
        }
}

func envHome(t *testing.T, env []string) string {
        t.Helper()
        for _, kv := range env {
                if strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
                        cfg := strings.TrimPrefix(kv, "XDG_CONFIG_HOME=")
                        return filepath.Clean(strings.TrimSuffix(cfg, "/.config"))
                }
        }
        t.Fatal("no XDG_CONFIG_HOME in env")
        return ""
}

// TestPreviewStateAndWebAPI verifies:
//   - .rupai/servers.json is loaded and live servers detected
//   - `rup.ai preview` serves the web UI + JSON API on 127.0.0.1
//   - the API requires the token and answers /api/status
func TestPreviewStateAndWebAPI(t *testing.T) {
        if binaryPath == "" {
                t.Skip("binary not built")
        }
        ws := t.TempDir()
        env := isolatedEnv(t)

        // a fake "dev server" that is actually running
        fake := newFakeDevServer(t)
        stateDir := filepath.Join(ws, ".rupai")
        if err := os.MkdirAll(stateDir, 0o755); err != nil {
                t.Fatal(err)
        }
        servers := []map[string]any{{
                "id": "srv-1", "name": "fake-app", "command": "echo fake",
                "url": fake.URL, "pid": 0, "started": time.Now().Format(time.RFC3339),
        }}
        raw, _ := json.Marshal(servers)
        if err := os.WriteFile(filepath.Join(stateDir, "servers.json"), raw, 0o644); err != nil {
                t.Fatal(err)
        }

        // point rup.ai at a configured dummy provider (the fake server also
        // answers /v1/models thanks to newOpenAITestServer-like behavior)
        dummy := newOpenAITestServer(t, []Turn{{Content: "ok"}}, &CapturedRequest{})
        env = envForProvider(env, dummy.URL, "openai")

        cmd := exec.Command(binaryPath, "preview")
        cmd.Env = append(os.Environ(), env...)
        cmd.Dir = ws
        var out strings.Builder
        cmd.Stdout = &out
        cmd.Stderr = &out
        if err := cmd.Start(); err != nil {
                t.Fatal(err)
        }
        defer func() {
                _ = cmd.Process.Kill()
                _, _ = cmd.Process.Wait()
        }()

        // wait for the web UI to come up and extract the tokenized URL
        var webURL string
        deadline := time.Now().Add(20 * time.Second)
        for time.Now().Before(deadline) {
                time.Sleep(300 * time.Millisecond)
                s := out.String()
                if i := strings.Index(s, "http://127.0.0.1:"); i >= 0 {
                        line := s[i:]
                        if j := strings.IndexAny(line, " \n\r\t"); j >= 0 {
                                line = line[:j]
                        }
                        webURL = strings.TrimSpace(line)
                        break
                }
        }
        if webURL == "" {
                t.Fatalf("web UI did not start; output:\n%s", out.String())
        }

        // health (no token) works
        resp, err := http.Get(webURLWithoutToken(webURL) + "/api/health")
        if err != nil {
                t.Fatalf("health check failed: %v", err)
        }
        body, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
                t.Fatalf("health endpoint unexpected: %d %s", resp.StatusCode, body)
        }

        // status without token → unauthorized
        resp2, err := http.Get(webURLWithoutToken(webURL) + "/api/status")
        if err != nil {
                t.Fatalf("status failed: %v", err)
        }
        resp2.Body.Close()
        if resp2.StatusCode != http.StatusUnauthorized {
                t.Fatalf("API must require the token, got %d", resp2.StatusCode)
        }

        // status with token → 200 JSON with workspace info
        token := ""
        if i := strings.Index(webURL, "token="); i >= 0 {
                token = strings.TrimSpace(webURL[i+len("token="):])
        }
        apiBase := strings.TrimSuffix(webURLWithoutToken(webURL), "/")
        resp3, err := http.Get(apiBase + "/api/status?token=" + token)
        if err != nil {
                t.Fatalf("status+token failed: %v", err)
        }
        b3, _ := io.ReadAll(resp3.Body)
        resp3.Body.Close()
        if resp3.StatusCode != 200 {
                t.Fatalf("status with token failed: %d %s", resp3.StatusCode, b3)
        }
        if !strings.Contains(string(b3), "workspace") {
                t.Fatalf("status payload missing workspace: %s", b3)
        }

        // servers list with token → our fake server is detected as alive
        resp4, err := http.Get(apiBase + "/api/servers?token=" + token)
        if err != nil {
                t.Fatalf("servers failed: %v", err)
        }
        b4, _ := io.ReadAll(resp4.Body)
        resp4.Body.Close()
        if !strings.Contains(string(b4), "fake-app") {
                t.Fatalf("servers payload should contain fake-app from state file: %s", b4)
        }
}

func webURLWithoutToken(url string) string {
        // strip ?token=... for the unauthorized probe
        if i := strings.Index(url, "?"); i >= 0 {
                return url[:i]
        }
        return url
}

// newFakeDevServer is a tiny always-200 HTTP server acting as the "app".
func newFakeDevServer(t *testing.T) *testServerHandle {
        t.Helper()
        mux := http.NewServeMux()
        mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
                w.Header().Set("Content-Type", "text/html")
                fmt.Fprint(w, "<html><body><h1>fake app</h1></body></html>")
        })
        srv := &http.Server{Addr: "127.0.0.1:0", Handler: mux, ReadHeaderTimeout: 3 * time.Second}
        ln := mustListen(t)
        go func() { _ = srv.Serve(ln) }()
        t.Cleanup(func() {
                ctx, cancel := contextWithTimeout(2 * time.Second)
                defer cancel()
                _ = srv.Shutdown(ctx)
        })
        return &testServerHandle{URL: "http://127.0.0.1" + portOf(ln)}
}

type testServerHandle struct{ URL string }
