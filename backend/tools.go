package main

// tools.go — the agent tool registry: real filesystem operations, command
// execution with security gating, git integration, patch application,
// change tracking with recoverable backups and unified diffs.

import (
        "context"
        "crypto/sha256"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "io"
        "os"
        "os/exec"
        "path/filepath"
        "regexp"
        "sort"
        "strings"
        "sync"
        "time"
)

// Change records one workspace modification performed this session.
type Change struct {
        Tool string `json:"tool"`
        Path string `json:"path"`
        Wrote bool  `json:"wrote"`
}

// Backup records a pre-change snapshot for /undo and restore_backup.
type Backup struct {
        Path       string    `json:"path"` // original (relative)
        BackupPath string    `json:"backup_path"`
        Time       time.Time `json:"time"`
}

// ChangeSet tracks session changes and backups.
type ChangeSet struct {
        mu      sync.Mutex
        Changed []Change
        Diffs   []string
        Backups []Backup
}

// Record appends a change entry.
func (c *ChangeSet) Record(ch Change, diff string) {
        c.mu.Lock()
        defer c.mu.Unlock()
        c.Changed = append(c.Changed, ch)
        if diff != "" {
                c.Diffs = append(c.Diffs, diff)
        }
}

// Tool is one callable agent capability.
type Tool struct {
        Name        string
        Description string
        Parameters  map[string]any
        ReadOnly    bool // safe: reads workspace or runs harmless git queries
        Fn          func(ctx context.Context, args string) (string, error)
}

// ToolRegistry wires tools to a workspace, policies and an approver.
type ToolRegistry struct {
        ws       *Workspace
        cfg      *Config
        changes  *ChangeSet
        approve  func(title, detail string, risk Risk) bool // interactive approver
        servers  *ServerManager
        Out      io.Writer // stream tool progress lines
}

// NewToolRegistry constructs the registry; approver may be nil (non-interactive).
func NewToolRegistry(ws *Workspace, cfg *Config, changes *ChangeSet, servers *ServerManager,
        approve func(title, detail string, risk Risk) bool, out io.Writer) *ToolRegistry {
        if out == nil {
                out = io.Discard
        }
        return &ToolRegistry{ws: ws, cfg: cfg, changes: changes, approve: approve, servers: servers, Out: out}
}

func boolSchema(desc string) map[string]any {
        return map[string]any{"type": "boolean", "description": desc}
}
func strSchema(desc string) map[string]any {
        return map[string]any{"type": "string", "description": desc}
}
func intSchema(desc string) map[string]any {
        return map[string]any{"type": "integer", "description": desc}
}

// Defs returns provider-agnostic tool definitions. readToolsOnly restricts to
// non-mutating tools (ask/plan/review modes).
func (t *ToolRegistry) Defs(readToolsOnly bool) []ToolDef {
        var defs []ToolDef
        for _, tool := range t.all() {
                if readToolsOnly && !tool.ReadOnly {
                        continue
                }
                defs = append(defs, ToolDef{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
        }
        return defs
}

// Names lists tool names.
func (t *ToolRegistry) Names() []string {
        var out []string
        for _, tool := range t.all() {
                out = append(out, tool.Name)
        }
        return out
}

// all constructs the tool table. Kept in one place for auditability.
func (t *ToolRegistry) all() []Tool {
        tools := []Tool{
                {
                        Name: "list_files", ReadOnly: true,
                        Description: "List files in a directory of the workspace (relative path, \".\" for root).",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path": strSchema("relative directory path")},
                                "required": []string{"path"}},
                        Fn: t.listFiles,
                },
                {
                        Name: "read_file", ReadOnly: true,
                        Description: "Read a text file from the workspace. Large files are truncated. Secret-looking files are redacted.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path": strSchema("relative file path")},
                                "required": []string{"path"}},
                        Fn: t.readFile,
                },
                {
                        Name: "search_text", ReadOnly: true,
                        Description: "Search file contents with a regular expression across the workspace (gitignore-aware).",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "pattern":  strSchema("regular expression"),
                                "glob":     strSchema("optional filename glob filter, e.g. *.go")},
                                "required": []string{"pattern"}},
                        Fn: t.searchText,
                },
                {
                        Name: "search_files", ReadOnly: true,
                        Description: "Find files whose names match a glob pattern.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "pattern": strSchema("glob, e.g. *.test.ts")},
                                "required": []string{"pattern"}},
                        Fn: t.searchFiles,
                },
                {
                        Name: "project_tree", ReadOnly: true,
                        Description: "Show the workspace file tree (bounded).",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.projectTree,
                },
                {
                        Name: "read_manifest", ReadOnly: true,
                        Description: "Read the project manifest (package.json, go.mod, requirements.txt, ...) if present.",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.readManifest,
                },
                {
                        Name: "detect_project", ReadOnly: true,
                        Description: "Detect languages, frameworks, package manager and common build/test/dev commands.",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.detectProject,
                },
                {
                        Name: "find_symbol", ReadOnly: true,
                        Description: "Find symbol definitions (functions, classes, structs, types) matching a name across the workspace.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "name": strSchema("symbol name (regular expression allowed)")},
                                "required": []string{"name"}},
                        Fn: t.findSymbol,
                },
                {
                        Name: "git_status", ReadOnly: true,
                        Description: "Show git status of the workspace repository (porcelain).",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.gitStatus,
                },
                {
                        Name: "git_diff", ReadOnly: true,
                        Description: "Show the current uncommitted git diff.",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.gitDiff,
                },
                {
                        Name: "git_log", ReadOnly: true,
                        Description: "Show recent git log (oneline, last 20 commits).",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.gitLog,
                },
                {
                        Name: "write_file",
                        Description: "Create or overwrite a file in the workspace. A backup is kept for undo. Prefer edit_file for small changes.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path":    strSchema("relative file path"),
                                "content": strSchema("full file content")},
                                "required": []string{"path", "content"}},
                        Fn: t.writeFile,
                },
                {
                        Name: "edit_file",
                        Description: "Targeted edit: replace an exact occurrence (or all occurrences) of old_text with new_text in a file. Prefer over rewriting files.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path":     strSchema("relative file path"),
                                "old_text": strSchema("exact text to replace"),
                                "new_text": strSchema("replacement text"),
                                "all":      boolSchema("replace every occurrence (default false: first only, must be unique)")},
                                "required": []string{"path", "old_text", "new_text"}},
                        Fn: t.editFile,
                },
                {
                        Name: "apply_patch",
                        Description: "Apply a unified diff (---/+++ @@ hunks) to workspace files. Use for multi-hunk edits.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "patch": strSchema("unified diff text")},
                                "required": []string{"patch"}},
                        Fn: t.applyPatch,
                },
                {
                        Name: "create_dir",
                        Description: "Create a directory (with parents) inside the workspace.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path": strSchema("relative directory path")},
                                "required": []string{"path"}},
                        Fn: t.createDir,
                },
                {
                        Name: "move_path",
                        Description: "Move or rename a file or directory inside the workspace.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "from": strSchema("source path"), "to": strSchema("destination path")},
                                "required": []string{"from", "to"}},
                        Fn: t.movePath,
                },
                {
                        Name: "delete_file",
                        Description: "Delete a file (moved to rup.ai backups first, recoverable). Never use for wholesale destruction.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path": strSchema("relative file path")},
                                "required": []string{"path"}},
                        Fn: t.deleteFile,
                },
                {
                        Name: "run_command",
                        Description: "Execute a shell command in the workspace and return stdout, stderr and exit code. Long-running dev servers: pass background=true. Commands are security-screened and may require user approval.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "command":    strSchema("shell command string"),
                                "background": boolSchema("run as managed background dev server and return its id")},
                                "required": []string{"command"}},
                        Fn: t.runCommand,
                },
                {
                        Name: "git_add",
                        Description: "Stage files in git (no commit, no push).",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
                                        "description": "relative paths to stage, or [\".\"] for all"}},
                                "required": []string{"paths"}},
                        Fn: t.gitAdd,
                },
                {
                        Name: "git_commit",
                        Description: "Commit staged changes with a message. Never commits without user-visible diff; never pushes.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "message": strSchema("commit message")},
                                "required": []string{"message"}},
                        Fn: t.gitCommit,
                },
                {
                        Name: "restore_backup",
                        Description: "Restore the most recent backup of a file changed this session.",
                        Parameters: map[string]any{"type": "object", "properties": map[string]any{
                                "path": strSchema("relative file path")},
                                "required": []string{"path"}},
                        Fn: t.restoreBackup,
                },
                {
                        Name: "list_servers", ReadOnly: true,
                        Description: "List managed background dev servers with their local URLs.",
                        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
                        Fn:          t.listServers,
                },
        }
        return tools
}

// Call executes a tool by name with a JSON argument object.
func (t *ToolRegistry) Call(ctx context.Context, name, args string) (string, error) {
        for _, tool := range t.all() {
                if tool.Name == name {
                        return tool.Fn(ctx, args)
                }
        }
        return "", fmt.Errorf("unknown tool %q", name)
}

// ---------- arg helpers ----------------------------------------------------

type argMap map[string]any

func parseArgs(args string) (argMap, error) {
        m := argMap{}
        if strings.TrimSpace(args) == "" {
                return m, nil
        }
        if err := json.Unmarshal([]byte(args), &m); err != nil {
                return nil, fmt.Errorf("invalid tool arguments: %w", err)
        }
        return m, nil
}

func (m argMap) str(key string) string {
        if v, ok := m[key]; ok {
                if s, ok := v.(string); ok {
                        return s
                }
        }
        return ""
}
func (m argMap) boolean(key string) bool {
        if v, ok := m[key]; ok {
                if b, ok := v.(bool); ok {
                        return b
                }
        }
        return false
}

// guard resolves and jails a path, classifies the write and asks for approval
// when policy requires it.
func (t *ToolRegistry) guardWrite(tool, rel string) (string, error) {
        abs, err := t.ws.Resolve(rel)
        if err != nil {
                Audit("blocked", t.ws.Root, "blocked", map[string]string{"tool": tool, "path": rel, "reason": err.Error()})
                return "", fmt.Errorf("refused: %v", err)
        }
        if denied, why := IsDeniedPath(abs); denied {
                Audit("blocked", t.ws.Root, "blocked", map[string]string{"tool": tool, "path": rel, "reason": why})
                return "", fmt.Errorf("refused: %s is protected (%s)", rel, why)
        }
        risk := RiskConfirm
        if _, err := os.Stat(abs); err == nil {
                risk = RiskConfirm
        }
        if policyAsks(t.cfg.Policies.Approval, risk) && t.approve != nil {
                ok := t.approve(fmt.Sprintf("%s → %s", tool, t.ws.Rel(abs)), "Modifies your workspace", risk)
                Audit("approval", t.ws.Root, map[bool]string{true: "approved", false: "denied"}[ok],
                        map[string]string{"tool": tool, "path": rel})
                if !ok {
                        return "", fmt.Errorf("user declined the change to %s", rel)
                }
        }
        return abs, nil
}

func (t *ToolRegistry) progress(line string) {
        fmt.Fprintln(t.Out, line)
}

// backupFile copies the current content of abs into the user-level backup dir
// before it is modified. Returns the backup path ("" when the file is new).
func (t *ToolRegistry) backupFile(abs string) (string, error) {
        raw, err := os.ReadFile(abs)
        if err != nil {
                if os.IsNotExist(err) {
                        return "", nil
                }
                return "", err
        }
        dir, err := DataDir()
        if err != nil {
                return "", err
        }
        sum := sha256.Sum256([]byte(strings.ToLower(t.ws.Root)))
        h := hex.EncodeToString(sum[:])[:12]
        bdir := filepath.Join(dir, "backups", h)
        if err := os.MkdirAll(bdir, 0o700); err != nil {
                return "", err
        }
        name := fmt.Sprintf("%s__%s", time.Now().Format("20060102-150405"), filepath.Base(abs))
        bpath := filepath.Join(bdir, name)
        if err := os.WriteFile(bpath, raw, 0o600); err != nil {
                return "", err
        }
        t.changes.mu.Lock()
        t.changes.Backups = append(t.changes.Backups, Backup{
                Path: t.ws.Rel(abs), BackupPath: bpath, Time: time.Now()})
        t.changes.mu.Unlock()
        return bpath, nil
}

// ---------- filesystem tools ----------------------------------------------

func (t *ToolRegistry) listFiles(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        rel := m.str("path")
        if rel == "" {
                rel = "."
        }
        abs, err := t.ws.Resolve(rel)
        if err != nil {
                return "", fmt.Errorf("refused: %v", err)
        }
        entries, err := os.ReadDir(abs)
        if err != nil {
                return "", err
        }
        var lines []string
        for _, e := range entries {
                name := e.Name()
                childRel := t.ws.Rel(filepath.Join(abs, name))
                if t.ws.IsIgnored(childRel, e.IsDir()) {
                        continue
                }
                suffix := "/"
                if !e.IsDir() {
                        suffix = ""
                }
                lines = append(lines, name+suffix)
        }
        sort.Strings(lines)
        if len(lines) == 0 {
                return "(empty directory)", nil
        }
        return strings.Join(lines, "\n"), nil
}

func (t *ToolRegistry) readFile(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        abs, err := t.ws.Resolve(m.str("path"))
        if err != nil {
                return "", fmt.Errorf("refused: %v", err)
        }
        if denied, why := IsDeniedPath(abs); denied {
                return "", fmt.Errorf("refused: protected file (%s)", why)
        }
        maxLines := t.cfg.Context.MaxFileLines
        if maxLines <= 0 {
                maxLines = 1600
        }
        content, err := ReadFileBounded(abs, maxLines)
        if err != nil {
                return "", err
        }
        if content == "" {
                return "(empty file)", nil
        }
        return content, nil
}

func (t *ToolRegistry) searchText(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        pattern := m.str("pattern")
        if pattern == "" {
                return "", fmt.Errorf("empty pattern")
        }
        re, err := regexp.Compile(pattern)
        if err != nil {
                return "", fmt.Errorf("invalid regex: %v", err)
        }
        var globRe *regexp.Regexp
        if g := m.str("glob"); g != "" {
                globRe = patternToRegexp(g)
        }
        files := t.ws.Scan(20000)
        limit := 120
        var out []string
        for _, f := range files {
                if globRe != nil && !globRe.MatchString(filepath.Base(f.Path)) {
                        continue
                }
                if f.Size > 2<<20 {
                        continue
                }
                abs := filepath.Join(t.ws.Root, filepath.FromSlash(f.Path))
                raw, err := os.ReadFile(abs)
                if err != nil {
                        continue
                }
                content := RedactFileContent(f.Path, string(raw))
                for i, line := range strings.Split(content, "\n") {
                        if re.MatchString(line) {
                                out = append(out, fmt.Sprintf("%s:%d: %s", f.Path, i+1, strings.TrimSpace(RedactString(line))))
                                if len(out) >= limit {
                                        out = append(out, "… (result limit reached)")
                                        return strings.Join(out, "\n"), nil
                                }
                        }
                }
        }
        if len(out) == 0 {
                return "(no matches)", nil
        }
        return strings.Join(out, "\n"), nil
}

func (t *ToolRegistry) searchFiles(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        re := patternToRegexp(m.str("pattern"))
        var out []string
        for _, f := range t.ws.Scan(20000) {
                if re.MatchString(filepath.Base(f.Path)) {
                        out = append(out, f.Path)
                        if len(out) >= 200 {
                                break
                        }
                }
        }
        if len(out) == 0 {
                return "(no matches)", nil
        }
        return strings.Join(out, "\n"), nil
}

func (t *ToolRegistry) projectTree(_ context.Context, _ string) (string, error) {
        return t.ws.Tree(t.cfg.Context.TreeMaxLines), nil
}

func (t *ToolRegistry) readManifest(_ context.Context, _ string) (string, error) {
        for _, name := range []string{"package.json", "go.mod", "requirements.txt", "pyproject.toml",
                "Cargo.toml", "composer.json", "pom.xml", "Gemfile", "Makefile"} {
                abs := filepath.Join(t.ws.Root, name)
                if raw, err := os.ReadFile(abs); err == nil {
                        content, _ := ReadFileBounded(abs, 200)
                        _ = raw
                        return name + ":\n" + content, nil
                }
        }
        return "(no known manifest found)", nil
}

func (t *ToolRegistry) detectProject(_ context.Context, _ string) (string, error) {
        raw, err := json.MarshalIndent(t.ws.Info, "", "  ")
        if err != nil {
                return "", err
        }
        return string(raw), nil
}

var symbolPatterns = map[string]*regexp.Regexp{
        ".go":   regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)|^type\s+([A-Za-z_][A-Za-z0-9_]*)`),
        ".py":   regexp.MustCompile(`^def\s+([A-Za-z_][A-Za-z0-9_]*)|^class\s+([A-Za-z_][A-Za-z0-9_]*)`),
        ".ts":   regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)|^(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)|^(?:export\s+)?interface\s+([A-Za-z_$][A-Za-z0-9_$]*)|^(?:export\s+)?type\s+([A-Za-z_$][A-Za-z0-9_$]*)`),
        ".tsx":  nil, // falls through to .ts rule
        ".js":   regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)|^(?:export\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`),
        ".java": regexp.MustCompile(`^\s*(?:public|private|protected)?\s*(?:class|interface|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`),
        ".rb":   regexp.MustCompile(`^def\s+([A-Za-z_][A-Za-z0-9_?!]*)|^class\s+([A-Za-z_][A-Za-z0-9_]*)`),
        ".rs":   regexp.MustCompile(`^\s*(?:pub\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)|^\s*(?:pub\s+)?(?:struct|enum|trait)\s+([A-Za-z_][A-Za-z0-9_]*)`),
}

func (t *ToolRegistry) findSymbol(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        nameRe, err := regexp.Compile("(?i)" + m.str("name"))
        if err != nil {
                return "", fmt.Errorf("invalid pattern: %v", err)
        }
        var out []string
        for _, f := range t.ws.Scan(20000) {
                ext := strings.ToLower(filepath.Ext(f.Path))
                if ext == ".tsx" {
                        ext = ".ts"
                }
                re, ok := symbolPatterns[ext]
                if !ok {
                        continue
                }
                abs := filepath.Join(t.ws.Root, filepath.FromSlash(f.Path))
                raw, err := os.ReadFile(abs)
                if err != nil {
                        continue
                }
                for i, line := range strings.Split(string(raw), "\n") {
                        if sub := re.FindStringSubmatch(line); sub != nil {
                                for _, g := range sub[1:] {
                                        if g != "" && nameRe.MatchString(g) {
                                                out = append(out, fmt.Sprintf("%s:%d: %s", f.Path, i+1, strings.TrimSpace(line)))
                                        }
                                }
                        }
                        if len(out) >= 100 {
                                break
                        }
                }
                if len(out) >= 100 {
                        out = append(out, "… (limit reached)")
                        break
                }
        }
        if len(out) == 0 {
                return "(no symbols found)", nil
        }
        return strings.Join(out, "\n"), nil
}

// write_file ---------------------------------------------------------------

func (t *ToolRegistry) writeFile(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        rel := m.str("path")
        content := m.str("content")
        abs, err := t.guardWrite("write_file", rel)
        if err != nil {
                return "", err
        }
        if _, err := t.backupFile(abs); err != nil {
                t.progress(fmt.Sprintf("backup skipped for %s: %v", rel, err))
        }
        before, _ := os.ReadFile(abs)
        if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
                return "", err
        }
        if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
                return "", err
        }
        diff := unifiedDiff(rel, string(before), content)
        t.changes.Record(Change{Tool: "write_file", Path: t.ws.Rel(abs), Wrote: true}, diff)
        t.ws.Invalidate()
        t.progress(fmt.Sprintf("wrote %s (%d bytes)", rel, len(content)))
        return fmt.Sprintf("wrote %s (%d bytes)%s", rel, len(content), diffHint(diff)), nil
}

func diffHint(diff string) string {
        if strings.Contains(diff, "+") && strings.Contains(diff, "-") {
                return " (modified)"
        }
        return ""
}

// edit_file ----------------------------------------------------------------

func (t *ToolRegistry) editFile(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        rel := m.str("path")
        oldText := m.str("old_text")
        newText := m.str("new_text")
        all := m.boolean("all")
        if oldText == "" {
                return "", fmt.Errorf("old_text must not be empty")
        }
        abs, err := t.guardWrite("edit_file", rel)
        if err != nil {
                return "", err
        }
        raw, err := os.ReadFile(abs)
        if err != nil {
                return "", fmt.Errorf("cannot read %s: %v (create it with write_file)", rel, err)
        }
        before := string(raw)
        count := strings.Count(before, oldText)
        if count == 0 {
                return "", fmt.Errorf("old_text not found in %s — read the file again and copy the exact text", rel)
        }
        if count > 1 && !all {
                return "", fmt.Errorf("old_text occurs %d times in %s; provide more surrounding context or set all=true", count, rel)
        }
        var after string
        if all {
                after = strings.ReplaceAll(before, oldText, newText)
        } else {
                after = strings.Replace(before, oldText, newText, 1)
        }
        if _, err := t.backupFile(abs); err != nil {
                t.progress(fmt.Sprintf("backup skipped for %s: %v", rel, err))
        }
        if err := os.WriteFile(abs, []byte(after), 0o644); err != nil {
                return "", err
        }
        diff := unifiedDiff(rel, before, after)
        t.changes.Record(Change{Tool: "edit_file", Path: t.ws.Rel(abs), Wrote: true}, diff)
        t.ws.Invalidate()
        repl := 1
        if all {
                repl = count
        }
        return fmt.Sprintf("edited %s (%d replacement%s)", rel, repl, plural(repl)) + diffHint(diff), nil
}

func plural(n int) string {
        if n == 1 {
                return ""
        }
        return "s"
}

// apply_patch --------------------------------------------------------------

var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@`)

func (t *ToolRegistry) applyPatch(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        patch := m.str("patch")
        results, err := t.applyUnifiedPatch(patch)
        if err != nil {
                return "", err
        }
        return results, nil
}

// applyUnifiedPatch parses a multi-file unified diff and applies it.
func (t *ToolRegistry) applyUnifiedPatch(patch string) (string, error) {
        lines := strings.Split(patch, "\n")
        type target struct {
                rel  string
                abs  string
                before string
                after  string
        }
        var cur *target
        var targets []*target
        var pos int
        var applied []string

        flush := func() error {
                if cur == nil {
                        return nil
                }
                if _, err := t.backupFile(cur.abs); err != nil {
                        t.progress(fmt.Sprintf("backup skipped for %s: %v", cur.rel, err))
                }
                if err := os.MkdirAll(filepath.Dir(cur.abs), 0o755); err != nil {
                        return err
                }
                if err := os.WriteFile(cur.abs, []byte(cur.after), 0o644); err != nil {
                        return err
                }
                diff := unifiedDiff(cur.rel, cur.before, cur.after)
                t.changes.Record(Change{Tool: "apply_patch", Path: cur.rel, Wrote: true}, diff)
                applied = append(applied, cur.rel)
                t.ws.Invalidate()
                cur = nil
                return nil
        }

        for pos < len(lines) {
                line := lines[pos]
                switch {
                case strings.HasPrefix(line, "--- "):
                        oldName := strings.TrimPrefix(line, "--- ")
                        var newName string
                        if pos+1 < len(lines) && strings.HasPrefix(lines[pos+1], "+++ ") {
                                newName = strings.TrimPrefix(lines[pos+1], "+++ ")
                                pos += 2
                        } else {
                                pos++
                        }
                        rel := patchTargetPath(oldName, newName)
                        abs, err := t.guardWrite("apply_patch", rel)
                        if err != nil {
                                return "", err
                        }
                        if err := flush(); err != nil {
                                return "", err
                        }
                        before, _ := os.ReadFile(abs)
                        cur = &target{rel: rel, abs: abs, before: string(before), after: string(before)}
                        targets = append(targets, cur)
                case strings.HasPrefix(line, "@@"):
                        if cur == nil {
                                return "", fmt.Errorf("hunk before file header")
                        }
                        pos++
                        var oldLines, newLines []string
                        for pos < len(lines) {
                                l := lines[pos]
                                if strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "diff ") {
                                        break
                                }
                                switch {
                                case strings.HasPrefix(l, "+"):
                                        newLines = append(newLines, strings.TrimPrefix(l, "+"))
                                case strings.HasPrefix(l, "-"):
                                        oldLines = append(oldLines, strings.TrimPrefix(l, "-"))
                                case strings.HasPrefix(l, " "):
                                        oldLines = append(oldLines, strings.TrimPrefix(l, " "))
                                        newLines = append(newLines, strings.TrimPrefix(l, " "))
                                case l == "":
                                        oldLines = append(oldLines, "")
                                        newLines = append(newLines, "")
                                }
                                pos++
                        }
                        after, err := applyHunk(cur.before, oldLines, newLines)
                        if err != nil {
                                return "", fmt.Errorf("patch failed for %s: %v", cur.rel, err)
                        }
                        cur.after = after
                default:
                        pos++
                }
        }
        if err := flush(); err != nil {
                return "", err
        }
        if len(applied) == 0 {
                return "", fmt.Errorf("no file headers found in patch — provide a proper unified diff with ---/+++ headers")
        }
        sort.Strings(applied)
        return "patch applied to: " + strings.Join(applied, ", "), nil
}

// patchTargetPath extracts the b/ path from a diff header.
func patchTargetPath(oldName, newName string) string {
        for _, n := range []string{newName, oldName} {
                n = strings.TrimSpace(n)
                n = strings.TrimSuffix(n, "\r")
                n = strings.TrimPrefix(n, "b/")
                n = strings.TrimPrefix(n, "a/")
                n = strings.TrimPrefix(n, "/dev/null")
                if n != "" && n != "/dev/null" {
                        return n
                }
        }
        return strings.TrimSpace(newName)
}

// applyHunk replaces the oldLines block with newLines in content.
func applyHunk(content string, oldLines, newLines []string) (string, error) {
        if len(oldLines) == 0 {
                return "", fmt.Errorf("empty hunk context")
        }
        oldBlock := strings.Join(oldLines, "\n")
        newBlock := strings.Join(newLines, "\n")
        if strings.Contains(content, oldBlock) {
                return strings.Replace(content, oldBlock, newBlock, 1), nil
        }
        // tolerate trailing-newline drift
        oldBlock = strings.TrimRight(oldBlock, "\n")
        newBlock = strings.TrimRight(newBlock, "\n")
        if strings.Contains(content, oldBlock) {
                return strings.Replace(content, oldBlock, newBlock, 1), nil
        }
        return "", fmt.Errorf("hunk context not found in file")
}

// create_dir ---------------------------------------------------------------

func (t *ToolRegistry) createDir(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        abs, err := t.guardWrite("create_dir", m.str("path"))
        if err != nil {
                return "", err
        }
        if err := os.MkdirAll(abs, 0o755); err != nil {
                return "", err
        }
        t.changes.Record(Change{Tool: "create_dir", Path: t.ws.Rel(abs), Wrote: true}, "")
        return "created directory " + t.ws.Rel(abs), nil
}

// move_path ----------------------------------------------------------------

func (t *ToolRegistry) movePath(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        src, err := t.ws.Resolve(m.str("from"))
        if err != nil {
                return "", fmt.Errorf("refused: %v", err)
        }
        dst, err := t.guardWrite("move_path", m.str("to"))
        if err != nil {
                return "", err
        }
        if _, err := os.Stat(src); err != nil {
                return "", fmt.Errorf("source not found: %s", m.str("from"))
        }
        if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
                return "", err
        }
        if err := os.Rename(src, dst); err != nil {
                return "", err
        }
        t.changes.Record(Change{Tool: "move_path", Path: t.ws.Rel(src) + " → " + t.ws.Rel(dst), Wrote: true}, "")
        t.ws.Invalidate()
        return fmt.Sprintf("moved %s → %s", t.ws.Rel(src), t.ws.Rel(dst)), nil
}

// delete_file --------------------------------------------------------------

func (t *ToolRegistry) deleteFile(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        rel := m.str("path")
        if rel == "" || rel == "." || rel == "/" {
                return "", fmt.Errorf("refusing to delete an empty/root path")
        }
        abs, err := t.ws.Resolve(rel)
        if err != nil {
                return "", fmt.Errorf("refused: %v", err)
        }
        st, err := os.Stat(abs)
        if err != nil {
                return "", err
        }
        if st.IsDir() {
                entries, _ := os.ReadDir(abs)
                if len(entries) > 0 {
                        return "", fmt.Errorf("refusing to delete non-empty directory %s (delete files individually)", rel)
                }
        } else if t.cfg.Policies.WorkspaceJail {
                if denied, why := IsDeniedPath(abs); denied {
                        return "", fmt.Errorf("refused: protected file (%s)", why)
                }
        }
        if policyAsks(t.cfg.Policies.Approval, RiskDangerous) && t.approve != nil {
                if !t.approve("delete_file → "+rel, "Removes a file from your workspace (backup kept)", RiskDangerous) {
                        Audit("approval", t.ws.Root, "denied", map[string]string{"tool": "delete_file", "path": rel})
                        return "", fmt.Errorf("user declined deletion of %s", rel)
                }
                Audit("approval", t.ws.Root, "approved", map[string]string{"tool": "delete_file", "path": rel})
        }
        bpath, _ := t.backupFile(abs)
        if err := os.Remove(abs); err != nil {
                return "", err
        }
        t.changes.Record(Change{Tool: "delete_file", Path: t.ws.Rel(abs), Wrote: true}, "")
        t.ws.Invalidate()
        msg := "deleted " + rel
        if bpath != "" {
                msg += " (backup: " + bpath + ")"
        }
        return msg, nil
}

// restore_backup -----------------------------------------------------------

func (t *ToolRegistry) restoreBackup(_ context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        rel := m.str("path")
        t.changes.mu.Lock()
        defer t.changes.mu.Unlock()
        for i := len(t.changes.Backups) - 1; i >= 0; i-- {
                b := t.changes.Backups[i]
                if rel == "" || b.Path == rel {
                        abs, err := t.ws.Resolve(b.Path)
                        if err != nil {
                                return "", err
                        }
                        raw, err := os.ReadFile(b.BackupPath)
                        if err != nil {
                                return "", fmt.Errorf("backup unreadable: %v", err)
                        }
                        if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
                                return "", err
                        }
                        if err := os.WriteFile(abs, raw, 0o644); err != nil {
                                return "", err
                        }
                        t.changes.Backups = append(t.changes.Backups[:i], t.changes.Backups[i+1:]...)
                        t.ws.Invalidate()
                        return "restored " + b.Path + " from backup", nil
                }
        }
        if rel != "" {
                return "", fmt.Errorf("no backup found for %s in this session", rel)
        }
        return "", fmt.Errorf("no backups in this session")
}

// run_command --------------------------------------------------------------

func (t *ToolRegistry) runCommand(ctx context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        cmdline := strings.TrimSpace(m.str("command"))
        if cmdline == "" {
                return "", fmt.Errorf("empty command")
        }
        risk, reason := ClassifyCommand(cmdline)
        if risk == RiskBlocked {
                Audit("blocked", t.ws.Root, "blocked", map[string]string{"command": cmdline, "reason": reason})
                return "", fmt.Errorf("command blocked by security policy (%s)", reason)
        }
        if policyAsks(t.cfg.Policies.Approval, risk) && t.approve != nil {
                ok := t.approve("run_command", cmdline, risk)
                Audit("command", t.ws.Root, map[bool]string{true: "approved", false: "denied"}[ok], map[string]string{"command": cmdline})
                if !ok {
                        return "", fmt.Errorf("user declined to run: %s", cmdline)
                }
        } else {
                Audit("command", t.ws.Root, "auto", map[string]string{"command": cmdline})
        }

        if m.boolean("background") {
                if t.servers == nil {
                        return "", fmt.Errorf("background servers unavailable")
                }
                srv, err := t.servers.Start(t.ws, "dev-server", cmdline)
                if err != nil {
                        return "", err
                }
                return fmt.Sprintf("started background process %s\ncommand: %s\n(state file: %s)",
                        srv.ID, cmdline, filepath.Join(t.ws.Root, ".rupai", "servers.json")), nil
        }

        timeout := time.Duration(t.cfg.Policies.CommandTimeoutSeconds) * time.Second
        if timeout <= 0 {
                timeout = 120 * time.Second
        }
        cctx, cancel := context.WithTimeout(ctx, timeout)
        defer cancel()

        shell, shellArgs := ShellForOS()
        cmd := exec.CommandContext(cctx, shell, append(shellArgs, cmdline)...)
        cmd.Dir = t.ws.Root
        cmd.Env = append(os.Environ(), "RUPAI_CHILD=1", "NO_COLOR=1", "CI=1")
        var stdout, stderr strings.Builder
        cmd.Stdout = &stdout
        cmd.Stderr = &stderr
        runErr := cmd.Run()
        exit := 0
        if runErr != nil {
                exit = 1
                if ee, ok := runErr.(*exec.ExitError); ok {
                        exit = ee.ExitCode()
                }
        }
        out := fmt.Sprintf("exit code: %d\n", exit)
        if so := strings.TrimSpace(stdout.String()); so != "" {
                if len(so) > 16000 {
                        so = so[:16000] + "\n… (stdout truncated)"
                }
                out += "stdout:\n" + RedactString(so) + "\n"
        }
        if se := strings.TrimSpace(stderr.String()); se != "" {
                if len(se) > 8000 {
                        se = se[:8000] + "\n… (stderr truncated)"
                }
                out += "stderr:\n" + RedactString(se) + "\n"
        }
        if exit != 0 {
                out += "note: the command failed; read the errors above and fix the likely cause."
        }
        return out, nil
}

// git tools ----------------------------------------------------------------

// git runs a git command through the OS shell (one string keeps quoting simple).
func (t *ToolRegistry) git(ctx context.Context, gitArgs ...string) (string, int, error) {
        shell, shellArgs := ShellForOS()
        cmdline := "git " + strings.Join(gitArgs, " ")
        cmd := exec.CommandContext(ctx, shell, append(shellArgs, cmdline)...)
        cmd.Dir = t.ws.Root
        var stdout, stderr strings.Builder
        cmd.Stdout = &stdout
        cmd.Stderr = &stderr
        err := cmd.Run()
        exit := 0
        if err != nil {
                exit = 1
                if ee, ok := err.(*exec.ExitError); ok {
                        exit = ee.ExitCode()
                }
        }
        return strings.TrimSpace(stdout.String()) + strings.TrimSpace(stderr.String()), exit, err
}

func (t *ToolRegistry) gitStatus(ctx context.Context, _ string) (string, error) {
        out, exit, _ := t.git(ctx, "status", "--porcelain=v1", "-b")
        if exit != 0 {
                return "", fmt.Errorf("git status failed: %s", out)
        }
        if out == "" {
                return "(clean working tree)", nil
        }
        return out, nil
}

func (t *ToolRegistry) gitDiff(ctx context.Context, _ string) (string, error) {
        out, exit, _ := t.git(ctx, "diff")
        if exit != 0 {
                return "", fmt.Errorf("git diff failed: %s", out)
        }
        if out == "" {
                return "(no unstaged changes)", nil
        }
        if len(out) > 12000 {
                out = out[:12000] + "\n… (diff truncated)"
        }
        return RedactString(out), nil
}

func (t *ToolRegistry) gitLog(ctx context.Context, _ string) (string, error) {
        out, exit, _ := t.git(ctx, "log", "--oneline", "-20")
        if exit != 0 {
                return "", fmt.Errorf("git log failed: %s", out)
        }
        return out, nil
}

func (t *ToolRegistry) gitAdd(ctx context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        var paths []string
        if v, ok := m["paths"]; ok {
                if arr, ok := v.([]any); ok {
                        for _, item := range arr {
                                if s, ok := item.(string); ok {
                                        paths = append(paths, s)
                                }
                        }
                }
        }
        if len(paths) == 0 {
                return "", fmt.Errorf("paths must be a non-empty array")
        }
        for _, p := range paths {
                if _, err := t.ws.Resolve(p); err != nil {
                        return "", fmt.Errorf("refused: %v", err)
                }
        }
        out, exit, _ := t.git(ctx, "add", "--", strings.Join(paths, " "))
        if exit != 0 {
                return "", fmt.Errorf("git add failed: %s", out)
        }
        return "staged: " + strings.Join(paths, " "), nil
}

func (t *ToolRegistry) gitCommit(ctx context.Context, args string) (string, error) {
        m, err := parseArgs(args)
        if err != nil {
                return "", err
        }
        msg := strings.TrimSpace(m.str("message"))
        if msg == "" {
                return "", fmt.Errorf("commit message required")
        }
        // refuse committing obvious secrets
        if re := regexp.MustCompile(`(?i)(api[_-]?key|secret|password|token)\s*[:=]`); re.MatchString(msg) {
                return "", fmt.Errorf("commit message looks like it contains a credential; rewrite it")
        }
        out, exit, _ := t.git(ctx, "commit", "-m", msg)
        if exit != 0 {
                return "", fmt.Errorf("git commit failed: %s", out)
        }
        Audit("git_commit", t.ws.Root, "approved", map[string]string{"message": msg})
        return "committed: " + strings.SplitN(out, "\n", 2)[0], nil
}

// list_servers -------------------------------------------------------------

func (t *ToolRegistry) listServers(_ context.Context, _ string) (string, error) {
        if t.servers == nil {
                return "(server manager unavailable)", nil
        }
        servers := t.servers.List()
        if len(servers) == 0 {
                return "(no background servers)", nil
        }
        var lines []string
        for _, s := range servers {
                status := "stopped"
                if s.Alive {
                        status = "running"
                }
                lines = append(lines, fmt.Sprintf("%s  %s  pid=%d  %s", s.ID, status, s.PID, s.URL))
        }
        return strings.Join(lines, "\n"), nil
}

// ---------- unified diff --------------------------------------------------

// unifiedDiff renders a bounded unified diff between two versions.
func unifiedDiff(rel, before, after string) string {
        const maxDiffLines = 400
        a := strings.Split(before, "\n")
        b := strings.Split(after, "\n")
        if len(a) > 4000 || len(b) > 4000 {
                return fmt.Sprintf("\n--- a/%s\n+++ b/%s\n(file too large for inline diff: %d → %d lines)\n", rel, rel, len(a), len(b))
        }
        ops := diffLines(a, b)
        if !hasRealChange(ops) {
                return ""
        }
        var out []string
        out = append(out, fmt.Sprintf("--- a/%s", rel), fmt.Sprintf("+++ b/%s", rel),
                fmt.Sprintf("@@ -1,%d +1,%d @@", len(a), len(b)))
        count := 0
        for _, op := range ops {
                switch {
                case op.del && op.add:
                        out = append(out, "-"+op.text, "+"+op.text)
                        count += 2
                case op.del:
                        out = append(out, "-"+op.text)
                        count++
                case op.add:
                        out = append(out, "+"+op.text)
                        count++
                default:
                        out = append(out, " "+op.text)
                        count++
                }
                if count >= maxDiffLines {
                        out = append(out, "… (diff truncated)")
                        break
                }
        }
        return strings.Join(out, "\n")
}

type diffOp struct {
        text     string
        del, add bool
}

func hasRealChange(ops []diffOp) bool {
        for _, op := range ops {
                if op.del != op.add {
                        return true
                }
        }
        return false
}

// diffLines computes a simple LCS-based line diff.
func diffLines(a, b []string) []diffOp {
        n, m := len(a), len(b)
        // guard against pathological sizes
        if n*m > 4_000_000 {
                return []diffOp{{text: "(large change; line-level diff skipped)", del: true, add: true}}
        }
        lcs := make([][]int, n+1)
        for i := range lcs {
                lcs[i] = make([]int, m+1)
        }
        for i := n - 1; i >= 0; i-- {
                for j := m - 1; j >= 0; j-- {
                        if a[i] == b[j] {
                                lcs[i][j] = lcs[i+1][j+1] + 1
                        } else if lcs[i+1][j] >= lcs[i][j+1] {
                                lcs[i][j] = lcs[i+1][j]
                        } else {
                                lcs[i][j] = lcs[i][j+1]
                        }
                }
        }
        var ops []diffOp
        i, j := 0, 0
        for i < n && j < m {
                switch {
                case a[i] == b[j]:
                        ops = append(ops, diffOp{text: a[i]})
                        i++
                        j++
                case lcs[i+1][j] >= lcs[i][j+1]:
                        ops = append(ops, diffOp{text: a[i], del: true, add: false})
                        i++
                default:
                        ops = append(ops, diffOp{text: b[j], add: true})
                        j++
                }
        }
        for ; i < n; i++ {
                ops = append(ops, diffOp{text: a[i], del: true})
        }
        for ; j < m; j++ {
                ops = append(ops, diffOp{text: b[j], add: true})
        }
        return ops
}

var _ = parseArgs // argMap helpers stay linked for tool call paths
