package main

// workspace.go — workspace resolution, project indexing, ignore rules,
// language/framework detection, @file reference expansion and token-aware
// context assembly. The agent never sends a whole project to the model:
// relevance-ranked excerpts are fitted into a configurable token budget.

import (
        "encoding/json"
        "fmt"
        "io/fs"
        "os"
        "path/filepath"
        "regexp"
        "sort"
        "strings"
        "time"
)

// FileEntry is one indexed file.
type FileEntry struct {
        Path    string  // relative, slash-separated
        Size    int64
        ModTime time.Time
}

// ProjectInfo summarizes detected stack and conventions.
type ProjectInfo struct {
        Languages      []string `json:"languages"`
        Frameworks     []string `json:"frameworks"`
        PackageManager string   `json:"package_manager,omitempty"`
        BuildCmd       string   `json:"build_cmd,omitempty"`
        TestCmd        string   `json:"test_cmd,omitempty"`
        DevCmd         string   `json:"dev_cmd,omitempty"`
        ManifestFile   string   `json:"manifest_file,omitempty"`
        GitRepo        bool     `json:"git_repo"`
}

// Workspace is the jailed working directory plus its index.
type Workspace struct {
        Root    string
        Info    ProjectInfo
        ignore  *ignoreMatcher
        files   []FileEntry
        scanned bool
}

// ResolveWorkspace resolves and validates the working directory.
func ResolveWorkspace(path string) (*Workspace, error) {
        root := path
        if root == "" {
                root, _ = os.Getwd()
        }
        abs, err := filepath.Abs(root)
        if err != nil {
                return nil, err
        }
        st, err := os.Stat(abs)
        if err != nil {
                return nil, fmt.Errorf("workspace %q is not accessible: %w", abs, err)
        }
        if !st.IsDir() {
                return nil, fmt.Errorf("workspace %q is not a directory", abs)
        }
        w := &Workspace{Root: abs}
        w.LoadIgnore()
        w.Info = w.Detect()
        return w, nil
}

// ---------- ignore rules ---------------------------------------------------

// ignoreMatcher holds gitignore-style patterns (subset: *, ?, **, !, dir/).
type ignoreMatcher struct {
        patterns []ignorePattern
}

type ignorePattern struct {
        negate  bool
        dirOnly bool
        pattern string
}

func newIgnoreMatcher() *ignoreMatcher { return &ignoreMatcher{} }

func (im *ignoreMatcher) add(line string) {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") {
                return
        }
        // strip trailing spaces not escaped (simplification: strip all)
        line = strings.TrimRight(line, " ")
        p := ignorePattern{}
        if strings.HasPrefix(line, "!") {
                p.negate = true
                line = line[1:]
        }
        if strings.HasPrefix(line, "/") {
                line = strings.TrimPrefix(line, "/")
        } else if !strings.Contains(line, "/") {
                line = "**/" + line
        }
        if strings.HasSuffix(line, "/") {
                p.dirOnly = true
                line = strings.TrimSuffix(line, "/")
        }
        if line != "" {
                p.pattern = line
                im.patterns = append(im.patterns, p)
        }
}

// match reports whether a slash-separated relative path is ignored.
func (im *ignoreMatcher) match(rel string, isDir bool) bool {
        ignored := false
        segs := strings.Split(rel, "/")
        // match any prefix so directories stop descending
        for i := 1; i <= len(segs); i++ {
                sub := strings.Join(segs[:i], "/")
                dirHere := isDir || i < len(segs)
                for _, p := range im.patterns {
                        if ok, _ := filepath.Match(p.pattern, sub); ok {
                                if p.dirOnly && !dirHere {
                                        continue
                                }
                                ignored = !p.negate
                        }
                        // pattern ending in /** or containing **
                        if strings.Contains(p.pattern, "**") {
                                rx := patternToRegexp(p.pattern)
                                if rx.MatchString(sub) && !(p.dirOnly && !dirHere) {
                                        ignored = !p.negate
                                }
                        }
                }
        }
        return ignored
}

func patternToRegexp(pattern string) *regexp.Regexp {
        var b strings.Builder
        b.WriteString("^")
        i := 0
        for i < len(pattern) {
                c := pattern[i]
                switch {
                case c == '*':
                        if i+1 < len(pattern) && pattern[i+1] == '*' {
                                // ** crosses directory boundaries
                                if i+2 < len(pattern) && pattern[i+2] == '/' {
                                        b.WriteString("(?:[^/]+/)*")
                                        i += 3
                                        continue
                                }
                                b.WriteString(".*")
                                i += 2
                                continue
                        }
                        b.WriteString("[^/]*")
                case c == '?':
                        b.WriteString("[^/]")
                default:
                        b.WriteString(regexp.QuoteMeta(string(c)))
                }
                i++
        }
        b.WriteString("$")
        re, err := regexp.Compile(b.String())
        if err != nil {
                return regexp.MustCompile(`^\z.`) // matches nothing
        }
        return re
}

// defaultIgnores always apply, even without a .gitignore.
var defaultIgnoreLines = []string{
        "node_modules/", ".git/", "__pycache__/", "*.pyc", ".venv/", "venv/",
        "dist/", "build/", "target/", "vendor/", ".next/", ".nuxt/", ".svelte-kit/",
        "bin/", "obj/", ".gradle/", ".idea/", ".vscode/", "coverage/", "*.min.js",
        "*.lock", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "go.sum",
        ".rupai/", "*.png", "*.jpg", "*.jpeg", "*.gif", "*.ico", "*.woff", "*.woff2",
        "*.ttf", "*.otf", "*.pdf", "*.zip", "*.tar.gz", "*.tgz", "*.whl", "*.jar",
        "*.class", "*.o", "*.so", "*.dylib", "*.dll", "*.exe",
}

func (w *Workspace) LoadIgnore() {
        w.ignore = newIgnoreMatcher()
        for _, line := range defaultIgnoreLines {
                w.ignore.add(line)
        }
        for _, name := range []string{".gitignore", ".rupaiignore"} {
                raw, err := os.ReadFile(filepath.Join(w.Root, name))
                if err != nil {
                        continue
                }
                for _, line := range strings.Split(string(raw), "\n") {
                        w.ignore.add(line)
                }
        }
}

// IsIgnored reports whether rel (slash-separated) is ignored.
func (w *Workspace) IsIgnored(rel string, isDir bool) bool {
        if rel == "." || rel == "" {
                return false
        }
        return w.ignore.match(rel, isDir)
}

// ---------- scanning -------------------------------------------------------

// Scan indexes the workspace (bounded) and caches the result.
func (w *Workspace) Scan(max int) []FileEntry {
        if w.scanned {
                return w.files
        }
        var files []FileEntry
        count := 0
        _ = filepath.WalkDir(w.Root, func(path string, d fs.DirEntry, err error) error {
                if err != nil {
                        return nil // unreadable entries are skipped silently
                }
                rel, err := filepath.Rel(w.Root, path)
                if err != nil {
                        return nil
                }
                if rel == "." {
                        return nil
                }
                rel = filepath.ToSlash(rel)
                if w.IsIgnored(rel, d.IsDir()) {
                        if d.IsDir() {
                                return filepath.SkipDir
                        }
                        return nil
                }
                if d.IsDir() {
                        return nil
                }
                if info, err := d.Info(); err == nil {
                        files = append(files, FileEntry{Path: rel, Size: info.Size(), ModTime: info.ModTime()})
                }
                count++
                if max > 0 && count >= max {
                        return fs.SkipAll
                }
                return nil
        })
        sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
        w.files = files
        w.scanned = true
        return files
}

// Invalidate resets the cached scan (called after writes).
func (w *Workspace) Invalidate() { w.scanned = false }

// Tree renders a bounded directory tree for the model.
func (w *Workspace) Tree(maxLines int) string {
        files := w.Scan(20000)
        var b strings.Builder
        b.WriteString(w.Root + "\n")
        lines := 1
        for _, f := range files {
                if lines >= maxLines {
                        b.WriteString("… (tree truncated, " + fmt.Sprint(len(files)) + " files total)\n")
                        break
                }
                b.WriteString("  " + f.Path + "  (" + fmt.Sprint(f.Size) + " bytes)\n")
                lines++
        }
        return b.String()
}

// ---------- jail -----------------------------------------------------------

// Resolve maps a model-supplied relative path to an absolute path inside the
// workspace, or returns an error when it escapes (jail).
func (w *Workspace) Resolve(rel string) (string, error) {
        rel = strings.TrimSpace(rel)
        rel = strings.TrimPrefix(rel, "~/")
        rel = strings.TrimPrefix(rel, "@")
        if rel == "" {
                return "", fmt.Errorf("empty path")
        }
        abs := rel
        if !filepath.IsAbs(abs) {
                abs = filepath.Join(w.Root, rel)
        }
        if err := InWorkspace(w.Root, abs); err != nil {
                return "", err
        }
        return abs, nil
}

// Rel converts an absolute path under the workspace to a display path.
func (w *Workspace) Rel(abs string) string {
        if rel, err := filepath.Rel(w.Root, abs); err == nil {
                return filepath.ToSlash(rel)
        }
        return abs
}

// ---------- detection ------------------------------------------------------

// Detect inspects manifests and infers the stack plus useful commands.
func (w *Workspace) Detect() ProjectInfo {
        info := ProjectInfo{}
        if st, err := os.Stat(filepath.Join(w.Root, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
                info.GitRepo = true
        }
        langSeen := map[string]bool{}
        addLang := func(l string) {
                if !langSeen[l] {
                        langSeen[l] = true
                        info.Languages = append(info.Languages, l)
                }
        }
        addFw := func(f string) {
                for _, have := range info.Frameworks {
                        if have == f {
                                return
                        }
                }
                info.Frameworks = append(info.Frameworks, f)
        }

        if raw, err := os.ReadFile(filepath.Join(w.Root, "package.json")); err == nil {
                info.ManifestFile = "package.json"
                addLang("JavaScript/TypeScript")
                var pkg struct {
                        Scripts      map[string]string `json:"scripts"`
                        Dependencies map[string]string `json:"dependencies"`
                        DevDeps      map[string]string `json:"devDependencies"`
                }
                if json.Unmarshal(raw, &pkg) == nil {
                        if pkg.Scripts["dev"] != "" {
                                info.DevCmd = "npm run dev"
                        }
                        if pkg.Scripts["build"] != "" {
                                info.BuildCmd = "npm run build"
                        }
                        if pkg.Scripts["test"] != "" {
                                info.TestCmd = "npm test"
                        }
                        fw := func(m map[string]string, names ...string) {
                                for _, n := range names {
                                        if _, ok := m[n]; ok {
                                                addFw(mapName(n))
                                        }
                                }
                        }
                        fw(pkg.Dependencies, "react", "next", "vue", "svelte", "@angular/core", "express", "fastify")
                        fw(pkg.DevDeps, "vite", "typescript", "jest", "vitest", "@angular/core")
                }
                if _, err := os.Stat(filepath.Join(w.Root, "pnpm-lock.yaml")); err == nil {
                        info.PackageManager = "pnpm"
                } else if _, err := os.Stat(filepath.Join(w.Root, "yarn.lock")); err == nil {
                        info.PackageManager = "yarn"
                } else if _, err := os.Stat(filepath.Join(w.Root, "bun.lockb")); err == nil {
                        info.PackageManager = "bun"
                } else {
                        info.PackageManager = "npm"
                }
        }
        if _, err := os.Stat(filepath.Join(w.Root, "go.mod")); err == nil {
                info.ManifestFile = orDefault(info.ManifestFile, "go.mod")
                addLang("Go")
                info.BuildCmd = orDefault(info.BuildCmd, "go build ./...")
                info.TestCmd = orDefault(info.TestCmd, "go test ./...")
        }
        if _, err := os.Stat(filepath.Join(w.Root, "requirements.txt")); err == nil {
                info.ManifestFile = orDefault(info.ManifestFile, "requirements.txt")
                addLang("Python")
                info.TestCmd = orDefault(info.TestCmd, "python -m pytest")
        }
        if _, err := os.Stat(filepath.Join(w.Root, "pyproject.toml")); err == nil {
                info.ManifestFile = orDefault(info.ManifestFile, "pyproject.toml")
                addLang("Python")
        }
        if raw, err := os.ReadFile(filepath.Join(w.Root, "requirements.txt")); err == nil {
                s := string(raw)
                if strings.Contains(s, "fastapi") {
                        addFw("FastAPI")
                }
                if strings.Contains(s, "django") {
                        addFw("Django")
                }
                if strings.Contains(s, "flask") {
                        addFw("Flask")
                }
        }
        if _, err := os.Stat(filepath.Join(w.Root, "Cargo.toml")); err == nil {
                info.ManifestFile = orDefault(info.ManifestFile, "Cargo.toml")
                addLang("Rust")
        }
        if _, err := os.Stat(filepath.Join(w.Root, "composer.json")); err == nil {
                info.ManifestFile = orDefault(info.ManifestFile, "composer.json")
                addLang("PHP")
        }
        // quick glob heuristics
        for pat, lang := range map[string]string{
                "*.go": "Go", "*.py": "Python", "*.ts": "TypeScript", "*.tsx": "TypeScript",
                "*.rs": "Rust", "*.java": "Java", "*.rb": "Ruby", "*.php": "PHP",
        } {
                if m, _ := filepath.Glob(filepath.Join(w.Root, pat)); len(m) > 0 {
                        addLang(lang)
                }
        }
        sort.Strings(info.Languages)
        return info
}

func mapName(dep string) string {
        switch dep {
        case "react":
                return "React"
        case "next":
                return "Next.js"
        case "vue":
                return "Vue"
        case "svelte":
                return "Svelte"
        case "@angular/core":
                return "Angular"
        case "express":
                return "Express"
        case "fastify":
                return "Fastify"
        case "vite":
                return "Vite"
        case "typescript":
                return "TypeScript"
        case "jest":
                return "Jest"
        case "vitest":
                return "Vitest"
        }
        return dep
}

func orDefault(cur, def string) string {
        if cur != "" {
                return cur
        }
        return def
}

// ---------- context assembly ----------------------------------------------

// EstimateTokens approximates token count (≈4 chars/token heuristic).
func EstimateTokens(s string) int { return len(s) / 4 }

// scoreFile ranks a file's relevance to a query.
func scoreFile(e FileEntry, query string) float64 {
        q := strings.ToLower(query)
        words := strings.Fields(q)
        score := 0.0
        lower := strings.ToLower(e.Path)
        base := strings.ToLower(filepath.Base(e.Path))
        for _, wd := range words {
                wd = strings.Trim(wd, "\"'`.,:;!?()[]{}")
                if len(wd) < 3 {
                        continue
                }
                if strings.Contains(base, wd) {
                        score += 6
                }
                if strings.Contains(lower, wd) {
                        score += 3
                }
        }
        // prefer docs and manifests a little
        for _, pref := range []string{"readme", "package.json", "go.mod", "requirements", "makefile", "main.", "index.", "app.", "server."} {
                if strings.HasPrefix(base, pref) {
                        score += 4
                }
        }
        // recency boost (up to 3 points for files touched today)
        if age := time.Since(e.ModTime); age < 24*time.Hour {
                score += 3
        } else if age < 7*24*time.Hour {
                score += 1
        }
        return score
}

// RelevantFiles returns the top-n files for a query.
func (w *Workspace) RelevantFiles(query string, n int) []FileEntry {
        files := w.Scan(20000)
        type scored struct {
                e FileEntry
                s float64
        }
        all := make([]scored, 0, len(files))
        for _, e := range files {
                all = append(all, scored{e, scoreFile(e, query)})
        }
        sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
        out := make([]FileEntry, 0, n)
        for i := 0; i < len(all) && i < n; i++ {
                out = append(out, all[i].e)
        }
        return out
}

// ReadFileBounded reads a file clipped to maxLines with a truncation marker.
func ReadFileBounded(path string, maxLines int) (string, error) {
        raw, err := os.ReadFile(path)
        if err != nil {
                return "", err
        }
        content := RedactFileContent(path, string(raw))
        content = RedactString(content)
        lines := strings.Split(content, "\n")
        if len(lines) > maxLines {
                return strings.Join(lines[:maxLines], "\n") +
                        fmt.Sprintf("\n… [truncated %d of %d lines]", len(lines)-maxLines, len(lines)), nil
        }
        return content, nil
}

// ExpandReferences rewrites @path tokens in the prompt to include file
// contents, respecting the token budget. Returns the expanded prompt and the
// list of referenced paths.
func (w *Workspace) ExpandReferences(text string, budgetTokens int) (string, []string) {
        re := regexp.MustCompile(`@([A-Za-z0-9_\-./\\]+)`)
        var refs []string
        var b strings.Builder
        remaining := budgetTokens
        last := 0
        for _, loc := range re.FindAllStringSubmatchIndex(text, -1) {
                path := text[loc[2]:loc[3]]
                abs, err := w.Resolve(path)
                if err != nil {
                        continue
                }
                st, err := os.Stat(abs)
                if err != nil {
                        continue
                }
                b.WriteString(text[last:loc[0]])
                last = loc[1]
                rel := w.Rel(abs)
                if st.IsDir() {
                        sub := &Workspace{Root: abs}
                        sub.ignore = w.ignore
                        b.WriteString("`" + rel + "`:\n" + sub.Tree(30))
                        refs = append(refs, rel)
                        continue
                }
                alloc := remaining / 3
                if alloc > 1200 {
                        alloc = 1200
                }
                lines := alloc * 4 / 60
                if lines < 40 {
                        lines = 40
                }
                content, err := ReadFileBounded(abs, lines)
                if err != nil {
                        b.WriteString(text[loc[0]:loc[1]])
                        continue
                }
                b.WriteString("`" + rel + "`:\n```\n" + content + "\n```\n")
                refs = append(refs, rel)
                remaining -= EstimateTokens(content)
                if remaining <= 0 {
                        break
                }
        }
        b.WriteString(text[last:])
        return b.String(), refs
}

// ProjectGuidance returns workspace instruction files (AGENTS.md / README).
func (w *Workspace) ProjectGuidance() string {
        var parts []string
        for _, name := range []string{"AGENTS.md", "rup.ai.md", "RUPAI.md"} {
                if raw, err := os.ReadFile(filepath.Join(w.Root, name)); err == nil {
                        parts = append(parts, "Instructions from "+name+":\n"+RedactString(string(raw)))
                        break
                }
        }
        if len(parts) == 0 {
                if raw, err := os.ReadFile(filepath.Join(w.Root, "README.md")); err == nil {
                        content := RedactString(string(raw))
                        if EstimateTokens(content) > 600 {
                                lines := strings.Split(content, "\n")
                                content = strings.Join(lines[:80], "\n") + "\n…"
                        }
                        parts = append(parts, "Excerpt from README.md:\n"+content)
                }
        }
        return strings.Join(parts, "\n\n")
}
