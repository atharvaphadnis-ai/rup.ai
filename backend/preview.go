package main

// preview.go — live application preview: managed background dev servers,
// port/URL detection, and the optional localhost companion web UI (serves
// frontend/dist and a documented JSON API for the browser interface).

import (
        "context"
        "crypto/rand"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "regexp"
        "runtime"
        "strings"
        "sync"
        "sync/atomic"
        "time"
)

// DevServer is a managed background process.
type DevServer struct {
        ID      string `json:"id"`
        Name    string `json:"name"`
        Command string `json:"command"`
        URL     string `json:"url"`
        PID     int    `json:"pid"`
        Started string `json:"started"`
        Alive   bool   `json:"alive"`
}

type serverSlot struct {
        meta   DevServer
        cmd    *exec.Cmd
        buf    ringBuffer
        exited int32 // atomic
}

// ServerManager tracks background dev servers for the workspace.
type ServerManager struct {
        mu      sync.Mutex
        ws      *Workspace
        servers map[string]*serverSlot
        counter int
}

// NewServerManager creates the manager and loads persisted state.
func NewServerManager(ws *Workspace) *ServerManager {
        m := &ServerManager{ws: ws, servers: map[string]*serverSlot{}}
        m.loadState()
        return m
}

func (m *ServerManager) statePath() string {
        return filepath.Join(m.ws.Root, ".rupai", "servers.json")
}

// ringBuffer keeps the last N output lines.
type ringBuffer struct {
        mu    sync.Mutex
        lines []string
}

func (r *ringBuffer) write(s string) {
        r.mu.Lock()
        defer r.mu.Unlock()
        r.lines = append(r.lines, strings.Split(strings.TrimRight(s, "\n"), "\n")...)
        if len(r.lines) > 800 {
                r.lines = r.lines[len(r.lines)-800:]
        }
}

func (r *ringBuffer) tail(n int) []string {
        r.mu.Lock()
        defer r.mu.Unlock()
        if n > len(r.lines) {
                n = len(r.lines)
        }
        out := make([]string, n)
        copy(out, r.lines[len(r.lines)-n:])
        return out
}

var urlPattern = regexp.MustCompile(`https?://(localhost|127\.0\.0\.1|\[::1\]):(\d+)`)

// Start launches a managed background process in the workspace.
func (m *ServerManager) Start(ws *Workspace, name, cmdline string) (*DevServer, error) {
        m.mu.Lock()
        defer m.mu.Unlock()
        shell, shellArgs := ShellForOS()
        cmd := exec.Command(shell, append(shellArgs, cmdline)...)
        cmd.Dir = ws.Root
        cmd.Env = append(os.Environ(), "RUPAI_CHILD=1", "FORCE_COLOR=0")
        slot := &serverSlot{cmd: cmd}
        stdout, err := cmd.StdoutPipe()
        if err != nil {
                return nil, err
        }
        cmd.Stderr = cmd.Stdout // merge
        if err := cmd.Start(); err != nil {
                return nil, fmt.Errorf("failed to start %q: %v", cmdline, err)
        }
        m.counter++
        id := fmt.Sprintf("srv-%d", m.counter)
        slot.meta = DevServer{
                ID: id, Name: name, Command: cmdline, PID: cmd.Process.Pid,
                Started: time.Now().Format(time.RFC3339),
        }
        m.servers[id] = slot

        go func() {
                reader := io.LimitReader(stdout, 64<<20)
                buf := make([]byte, 8192)
                for {
                        n, rerr := reader.Read(buf)
                        if n > 0 {
                                slot.buf.write(string(buf[:n]))
                                if slot.meta.URL == "" {
                                        if match := urlPattern.FindStringSubmatch(string(buf[:n])); match != nil {
                                                slot.meta.URL = fmt.Sprintf("http://127.0.0.1:%s", match[2])
                                        }
                                }
                        }
                        if rerr != nil {
                                break
                        }
                }
                slot.buf.write("[process exited]\n")
                atomic.StoreInt32(&slot.exited, 1)
        }()
        m.saveStateLocked()
        return &slot.meta, nil
}

// List returns all known servers with live status.
func (m *ServerManager) List() []DevServer {
        m.mu.Lock()
        defer m.mu.Unlock()
        out := make([]DevServer, 0, len(m.servers))
        for _, slot := range m.servers {
                slot.meta.Alive = m.alive(slot)
                out = append(out, slot.meta)
        }
        // sort by id
        for i := 1; i < len(out); i++ {
                for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
                        out[j], out[j-1] = out[j-1], out[j]
                }
        }
        return out
}

func (m *ServerManager) alive(slot *serverSlot) bool {
        if atomic.LoadInt32(&slot.exited) == 1 {
                return false
        }
        if slot.cmd == nil || slot.cmd.Process == nil {
                // loaded from a previous session's state file: probe by URL
                if slot.meta.URL != "" {
                        return probeHTTP(slot.meta.URL)
                }
                return false
        }
        return true
}

// Output returns the tail of a server's combined output.
func (m *ServerManager) Output(id string, lines int) (string, bool) {
        m.mu.Lock()
        slot, ok := m.servers[id]
        m.mu.Unlock()
        if !ok {
                return "", false
        }
        return strings.Join(slot.buf.tail(lines), "\n"), true
}

// Stop terminates a managed server.
func (m *ServerManager) Stop(id string) (bool, error) {
        m.mu.Lock()
        slot, ok := m.servers[id]
        m.mu.Unlock()
        if !ok {
                return false, fmt.Errorf("unknown server %s", id)
        }
        if slot.cmd != nil && slot.cmd.Process != nil {
                _ = slot.cmd.Process.Kill()
        }
        m.mu.Lock()
        delete(m.servers, id)
        m.saveStateLocked()
        m.mu.Unlock()
        return true, nil
}

// saveStateLocked persists the registry for later sessions (callers hold mu).
func (m *ServerManager) saveStateLocked() {
        list := make([]DevServer, 0, len(m.servers))
        for _, s := range m.servers {
                list = append(list, s.meta)
        }
        if err := os.MkdirAll(filepath.Dir(m.statePath()), 0o755); err != nil {
                return
        }
        raw, _ := json.MarshalIndent(list, "", "  ")
        _ = os.WriteFile(m.statePath(), raw, 0o644)
}

// loadState reads servers.json; entries are kept with alive=false until a
// running process claims them (detection via health probe).
func (m *ServerManager) loadState() {
        raw, err := os.ReadFile(m.statePath())
        if err != nil {
                return
        }
        var list []DevServer
        if json.Unmarshal(raw, &list) != nil {
                return
        }
        for _, s := range list {
                if s.URL == "" || !probeHTTP(s.URL) {
                        continue
                }
                m.counter++
                id := fmt.Sprintf("srv-%d", m.counter)
                s.ID = id
                s.Alive = true
                m.servers[id] = &serverSlot{meta: s}
        }
}

func probeHTTP(url string) bool {
        client := &http.Client{Timeout: 2 * time.Second}
        resp, err := client.Get(url)
        if err != nil {
                return false
        }
        resp.Body.Close()
        return resp.StatusCode < 500
}

// probePort reports whether a TCP port on 127.0.0.1 accepts connections.
func probePort(port int) bool {
        conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
        if err != nil {
                return false
        }
        conn.Close()
        return true
}

// OpenInBrowser opens a URL with the OS default browser (shell-based, no
// platform-specific syscalls, compiles everywhere).
func OpenInBrowser(url string) error { return openInBrowser(url) }

func openInBrowser(url string) error {
        switch {
        case isWindows():
                return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
        case runtime.GOOS == "darwin":
                return exec.Command("open", url).Start()
        default:
                if _, err := exec.LookPath("xdg-open"); err == nil {
                        return exec.Command("xdg-open", url).Start()
                }
                return fmt.Errorf("no browser launcher found (xdg-open missing)")
        }
}

// --------------------------------------------------------------------------
// Companion web UI
// --------------------------------------------------------------------------

// StatusPayload is served on /api/status.
type StatusPayload struct {
        Version     string   `json:"version"`
        Provider    string   `json:"provider"`
        Model       string   `json:"model"`
        Mode        string   `json:"mode"`
        Workspace   string   `json:"workspace"`
        UptimeSec   int64    `json:"uptime_sec"`
        UsageTokens int64    `json:"usage_tokens"`
        Steps       int      `json:"steps"`
        Providers   []string `json:"providers"`
}

// WebBridge supplies live data to the web UI (implemented by cli.go).
type WebBridge struct {
        Status   func() StatusPayload
        Config   func() *Config
        SaveConfig func(func(*Config)) error
        Models   func(ctx context.Context, providerID string) ([]string, error)
        Servers  func() []DevServer
        ServerOutput func(id string, n int) (string, bool)
        StopServer   func(id string) (bool, error)
        OpenBrowser  func(url string) error
        SetBusy  func(bool)
        Busy     func() bool
}

// webServer holds the running companion UI.
type webServer struct {
        srv   *http.Server
        token string
        start time.Time
}

// ServeWeb starts the companion web UI on 127.0.0.1 and returns its URL plus
// a shutdown function. It binds per WebCfg (default loopback only).
func ServeWeb(cfg *Config, bridge *WebBridge) (string, func(), error) {
        bind := cfg.Web.Bind
        if bind == "" {
                bind = "127.0.0.1"
        }
        port := cfg.Web.Port
        if port == 0 {
                port = 8618
        }
        token := randomToken()
        ws := &webServer{token: token, start: time.Now()}

        mux := http.NewServeMux()
        mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { ws.serveStatic(w, r) })
        mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { ws.health(w, r) })
        mux.HandleFunc("/api/status", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.status(w, r, bridge) }))
        mux.HandleFunc("/api/config", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.config(w, r, bridge) }))
        mux.HandleFunc("/api/models", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.models(w, r, bridge) }))
        mux.HandleFunc("/api/servers", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.servers(w, r, bridge) }))
        mux.HandleFunc("/api/servers/output", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.serverOutput(w, r, bridge) }))
        mux.HandleFunc("/api/servers/stop", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.serverStop(w, r, bridge) }))
        mux.HandleFunc("/api/open", ws.authed(func(w http.ResponseWriter, r *http.Request) { ws.openURL(w, r, bridge) }))

        addr := fmt.Sprintf("%s:%d", bind, port)
        ws.srv = &http.Server{Addr: addr, Handler: ws.guard(mux), ReadHeaderTimeout: 5 * time.Second}
        ln, err := net.Listen("tcp", addr)
        if err != nil {
                return "", nil, fmt.Errorf("cannot bind web UI on %s: %v", addr, err)
        }
        go func() { _ = ws.srv.Serve(ln) }()
        url := fmt.Sprintf("http://%s:%d/?token=%s", displayHost(bind), port, token)
        shutdown := func() {
                ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
                defer cancel()
                _ = ws.srv.Shutdown(ctx)
        }
        return url, shutdown, nil
}

func displayHost(bind string) string {
        if bind == "0.0.0.0" || bind == "" {
                return "127.0.0.1"
        }
        return bind
}

func randomToken() string {
        b := make([]byte, 18)
        if _, err := rand.Read(b); err != nil {
                return fmt.Sprintf("%d", time.Now().UnixNano())
        }
        const hexdigits = "0123456789abcdef"
        out := make([]byte, len(b)*2)
        for i, v := range b {
                out[i*2] = hexdigits[v>>4]
                out[i*2+1] = hexdigits[v&0x0f]
        }
        return string(out)
}

// guard adds localhost/CSRF protections to every request.
func (ws *webServer) guard(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                host := r.Host
                if i := strings.LastIndex(host, ":"); i > 0 && !strings.HasSuffix(host, "]") {
                        host = host[:i]
                }
                if host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1" {
                        http.Error(w, "rup.ai web UI is localhost-only by default", http.StatusForbidden)
                        return
                }
                if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
                        if o := r.Header.Get("Origin"); o != "" && !strings.Contains(o, "127.0.0.1") && !strings.Contains(o, "localhost") {
                                http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
                                return
                        }
                }
                w.Header().Set("X-Content-Type-Options", "nosniff")
                next.ServeHTTP(w, r)
        })
}

// authed requires the token for API routes (the UI receives it in the URL).
func (ws *webServer) authed(h http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
                token := r.URL.Query().Get("token")
                if token == "" {
                        token = r.Header.Get("X-Rupai-Token")
                }
                if token != ws.token {
                        http.Error(w, "unauthorized: open the web UI via the URL printed by /preview", http.StatusUnauthorized)
                        return
                }
                h(w, r)
        }
}

func writeJSON(w http.ResponseWriter, v any) {
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(v)
}

func (ws *webServer) health(w http.ResponseWriter, _ *http.Request) {
        writeJSON(w, map[string]any{"ok": true, "version": version})
}

func (ws *webServer) status(w http.ResponseWriter, _ *http.Request, b *WebBridge) {
        if b.Status != nil {
                writeJSON(w, b.Status())
                return
        }
        writeJSON(w, StatusPayload{Version: version})
}

func (ws *webServer) config(w http.ResponseWriter, r *http.Request, b *WebBridge) {
        switch r.Method {
        case http.MethodGet:
                if b.Config != nil {
                        writeJSON(w, b.Config().Redacted())
                }
        case http.MethodPut:
                var patch struct {
                        Provider string `json:"provider"`
                        Model    string `json:"model"`
                        Mode     string `json:"mode"`
                        Approval string `json:"approval"`
                        APIKey   string `json:"api_key"`
                }
                if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&patch); err != nil {
                        http.Error(w, "invalid body", http.StatusBadRequest)
                        return
                }
                err := b.SaveConfig(func(c *Config) {
                        if patch.Provider != "" {
                                c.ActiveProvider = patch.Provider
                        }
                        if patch.Model != "" {
                                c.ActiveModel = patch.Model
                        }
                        if patch.Approval != "" {
                                c.Policies.Approval = patch.Approval
                        }
                })
                if err != nil {
                        http.Error(w, err.Error(), http.StatusInternalServerError)
                        return
                }
                if patch.APIKey != "" && patch.Provider != "" {
                        _ = StoreSecret(patch.Provider, "api_key", patch.APIKey)
                }
                writeJSON(w, map[string]any{"ok": true})
        default:
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        }
}

func (ws *webServer) models(w http.ResponseWriter, r *http.Request, b *WebBridge) {
        id := r.URL.Query().Get("provider")
        if id == "" || b.Models == nil {
                http.Error(w, "provider parameter required", http.StatusBadRequest)
                return
        }
        ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
        defer cancel()
        models, err := b.Models(ctx, id)
        if err != nil {
                writeJSON(w, map[string]any{"error": err.Error(), "models": []string{}})
                return
        }
        writeJSON(w, map[string]any{"models": models})
}

func (ws *webServer) servers(w http.ResponseWriter, _ *http.Request, b *WebBridge) {
        if b.Servers != nil {
                writeJSON(w, map[string]any{"servers": b.Servers()})
                return
        }
        writeJSON(w, map[string]any{"servers": []DevServer{}})
}

func (ws *webServer) serverOutput(w http.ResponseWriter, r *http.Request, b *WebBridge) {
        id := r.URL.Query().Get("id")
        if b.ServerOutput == nil {
                http.Error(w, "unavailable", http.StatusServiceUnavailable)
                return
        }
        out, ok := b.ServerOutput(id, 100)
        if !ok {
                http.Error(w, "unknown server", http.StatusNotFound)
                return
        }
        writeJSON(w, map[string]any{"output": out})
}

func (ws *webServer) serverStop(w http.ResponseWriter, r *http.Request, b *WebBridge) {
        if r.Method != http.MethodPost {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
        }
        var body struct {
                ID string `json:"id"`
        }
        if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.ID == "" {
                http.Error(w, "id required", http.StatusBadRequest)
                return
        }
        if b.StopServer == nil {
                http.Error(w, "unavailable", http.StatusServiceUnavailable)
                return
        }
        ok, err := b.StopServer(body.ID)
        writeJSON(w, map[string]any{"ok": ok, "error": errString(err)})
}

func (ws *webServer) openURL(w http.ResponseWriter, r *http.Request, b *WebBridge) {
        if r.Method != http.MethodPost {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
        }
        var body struct {
                URL string `json:"url"`
        }
        if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.URL == "" {
                http.Error(w, "url required", http.StatusBadRequest)
                return
        }
        if !strings.HasPrefix(body.URL, "http://127.0.0.1") && !strings.HasPrefix(body.URL, "http://localhost") {
                http.Error(w, "only local URLs can be opened", http.StatusBadRequest)
                return
        }
        if b.OpenBrowser != nil {
                _ = b.OpenBrowser(body.URL)
        }
        writeJSON(w, map[string]any{"ok": true})
}

// serveStatic serves frontend/dist when it has been built (BUILD.md), with a
// helpful placeholder otherwise.
func (ws *webServer) serveStatic(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets/") {
                http.NotFound(w, r)
                return
        }
        for _, dir := range webDistCandidates() {
                idx := filepath.Join(dir, "index.html")
                if raw, err := os.ReadFile(idx); err == nil {
                        w.Header().Set("Content-Type", "text/html; charset=utf-8")
                        _, _ = w.Write(raw)
                        return
                }
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        fmt.Fprintf(w, `<!doctype html><html><head><title>rup.ai</title></head>
<body style="font-family:system-ui;background:#0b0e14;color:#e6e6e6;padding:3rem">
<h2>rup.ai web UI</h2>
<p>The companion frontend is not built yet. Run <code>npm install &amp;&amp; npm run build</code>
in the project root (see BUILD.md), then restart the preview.</p>
<p>The JSON API is available under <code>/api/…</code> with the token from the CLI.</p>
</body></html>`)
}

// webDistCandidates lists places to look for the built frontend.
func webDistCandidates() []string {
        var out []string
        if v := os.Getenv("RUPAI_WEB_DIST"); v != "" {
                out = append(out, v)
        }
        if exe, err := os.Executable(); err == nil {
                out = append(out, filepath.Join(filepath.Dir(exe), "web", "dist"))
        }
        if wd, err := os.Getwd(); err == nil {
                out = append(out, filepath.Join(wd, "frontend", "dist"))
                out = append(out, filepath.Join(wd, "web", "dist"))
        }
        if dir, err := ConfigDir(); err == nil {
                out = append(out, filepath.Join(dir, "web"))
        }
        if dir, err := DataDir(); err == nil {
                out = append(out, filepath.Join(dir, "web"))
        }
        return out
}

func errString(err error) string {
        if err == nil {
                return ""
        }
        return err.Error()
}
