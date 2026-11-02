package main

// security.go — the security layer: command risk classification, approval
// policies, workspace path jail, secret redaction and the audit log.
//
// Model-generated commands and paths are treated as untrusted input. The
// agent is jailed to the active workspace, destructive commands require
// explicit approval, and a small set of catastrophic patterns is always
// blocked regardless of policy.

import (
        "encoding/json"
        "fmt"
        "os"
        "path/filepath"
        "regexp"
        "runtime"
        "sort"
        "strings"
        "time"
)

// Risk classifies how dangerous an action is.
type Risk int

const (
        RiskSafe Risk = iota // read-only, reversible
        RiskConfirm          // writes within the workspace, normal commands
        RiskDangerous        // destructive or risky: needs explicit approval
        RiskBlocked          // never executed by rup.ai
)

func (r Risk) String() string {
        switch r {
        case RiskSafe:
                return "safe"
        case RiskConfirm:
                return "confirm"
        case RiskDangerous:
                return "dangerous"
        default:
                return "blocked"
        }
}

// Approval policies (Policies.Approval):
//   restricted  — confirm every command execution and every file write
//   standard    — auto-approve safe actions; confirm writes & risky commands
//   permissive  — auto-approve everything except dangerous/blocked actions
func policyAsks(approval string, risk Risk) bool {
        switch approval {
        case "restricted":
                return risk != RiskBlocked
        case "permissive":
                return risk == RiskDangerous
        default: // standard
                return risk == RiskConfirm || risk == RiskDangerous
        }
}

// blockedPatterns are catastrophic operations rup.ai refuses outright.
var blockedPatterns = []*regexp.Regexp{
        regexp.MustCompile(`(?i)\brm\s+(-[a-z]*[a-z]*\s+)*(-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*)\s+/(\s|$)`),               // rm -rf /
        regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-?[a-z]*r[a-z]*f[a-z]*\s+(~|\$HOME|\.\.)`),                                      // rm -rf ~ / .. / $HOME
        regexp.MustCompile(`(?i)\b(del|rd|rmdir)\s+(/s|/q)\b[^&]*\b([a-z]:\\|"%?(USERPROFILE|HOMEDRIVE|SystemRoot)%?)`),             // Windows mass delete
        regexp.MustCompile(`(?i)\b(format|mkfs(\.\w+)?|fdisk|diskpart|cipher\s+/w)\b`),                                              // disk wiping
        regexp.MustCompile(`(?i)\bdd\s+[^|]*\bof=/dev/(sd|nvme|hd|disk)`),                                                           // raw disk write
        regexp.MustCompile(`(?i):\(\)\s*\{\s*:\|:\s*&\s*\}\s*;\s*:`),                                                                // fork bomb
        regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b`),                                                               // power control
        regexp.MustCompile(`(?i)\breg\s+(delete|add)\s+HKLM`),                                                                       // registry tampering
        regexp.MustCompile(`(?i)\b(vssadmin|bcdedit|wevtutil)\b`),                                                                   // windows system tampering
        regexp.MustCompile(`(?i)\bchmod\s+-R?\s*777\s+/\s*$`),                                                                       // chmod everything
        regexp.MustCompile(`(?i)\b(mv|cp|move|copy)\b[^;&|]*\s+(/dev/sd|/dev/nvme)`),                                                // overwrite device
        regexp.MustCompile(`(?i)\bapt\s+.*\|?\s*(sudo\s+)?sh\b|\bcurl\b[^|]*\|\s*(ba)?sh\b|\bwget\b[^|]*\|\s*(ba)?sh\b`),            // curl|sh
        regexp.MustCompile(`(?i)\bdsquery|net\s+user\s+.*\s/add\b`),                                                                 // account creation
}

// dangerousPatterns require explicit approval even under permissive policy.
var dangerousPatterns = []*regexp.Regexp{
        regexp.MustCompile(`(?i)\brm\s+(-[a-zA-Z]+\s+) *-?[a-zA-Z]*r`),
        regexp.MustCompile(`(?i)\bdel\s+/(s|q)\b`),
        regexp.MustCompile(`(?i)\bgit\s+push\b[^;&|]*--force\b`),
        regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\b`),
        regexp.MustCompile(`(?i)\bgit\s+clean\s+-[a-z]*f`),
        regexp.MustCompile(`(?i)\bdrop\s+(database|table)\b`),
        regexp.MustCompile(`(?i)\btruncate\s+table\b`),
        regexp.MustCompile(`(?i)\bsudo\b`),
        regexp.MustCompile(`(?i)\bchmod\s+777\b`),
        regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|bun)\s+(run\s+)?publish\b`),
        regexp.MustCompile(`(?i)\bpip\s+install\b.*--break-system-packages\b`),
        regexp.MustCompile(`(?i)\btaskkill\s+/f\b|\bkill\s+-9\b`),
        regexp.MustCompile(`(?i)\bkillall\b`),
        regexp.MustCompile(`(?i)\benv\b|\bprintenv\b`), // may print credentials
        regexp.MustCompile(`(?i)\bssh\b|\bscp\b|\bsshpass\b`),
        regexp.MustCompile(`(?i)>\s*/etc/|\bsudoers\b`),
}

// safePrefixes are read-only or conventionally harmless commands that do not
// require approval under standard/permissive policies.
var safePrefixes = []string{
        "echo ", "ls", "pwd", "cat ", "head ", "tail ", "wc ", "which ", "type ",
        "where ", "date", "whoami", "tree", "file ", "stat ", "du ", "df ",
        "git status", "git log", "git diff", "git branch", "git show", "git rev-parse",
        "git remote", "git stash list", "git config --get",
        "node -v", "node --version", "npm -v", "npm --version", "npx tsc",
        "go version", "go env", "go build", "go test", "go vet", "go run", "gofmt",
        "python --version", "python3 --version", "python -m pytest", "pytest",
        "pip --version", "pip3 --version", "pip list", "pip3 list",
        "cargo build", "cargo test", "cargo check", "cargo --version",
        "rg ", "grep ", "findstr ", "find .", "flutter --version", "dotnet --version",
        "java -version", "javac -version", "ruby -v", "php -v", "composer --version",
}

// ClassifyCommand returns the risk of a shell command plus a reason.
func ClassifyCommand(cmd string) (Risk, string) {
        trimmed := strings.TrimSpace(cmd)
        if trimmed == "" {
                return RiskBlocked, "empty command"
        }
        for _, re := range blockedPatterns {
                if re.MatchString(trimmed) {
                        return RiskBlocked, "catastrophic or system-level operation"
                }
        }
        // split on shell separators and classify the most dangerous segment
        segments := splitCommand(trimmed)
        for _, seg := range segments {
                for _, re := range dangerousPatterns {
                        if re.MatchString(seg) {
                                return RiskDangerous, "potentially destructive: " + seg
                        }
                }
        }
        // safe only when every segment matches the allowlist
        allSafe := true
        for _, seg := range segments {
                if !hasSafePrefix(seg) {
                        allSafe = false
                        break
                }
        }
        if allSafe {
                return RiskSafe, ""
        }
        return RiskConfirm, ""
}

// hasSafePrefix checks one command segment against the allowlist.
func hasSafePrefix(seg string) bool {
        s := strings.TrimSpace(seg)
        s = strings.TrimLeft(s, "(")
        for _, p := range safePrefixes {
                if s == strings.TrimSpace(p) || strings.HasPrefix(s, p) {
                        return true
                }
        }
        return false
}

// splitCommand splits on shell separators; keeps words intact for inspection.
func splitCommand(cmd string) []string {
        fields := strings.FieldsFunc(cmd, func(r rune) bool {
                return r == ';' || r == '&' || r == '|' || r == '\n'
        })
        out := make([]string, 0, len(fields))
        for _, f := range fields {
                if s := strings.TrimSpace(f); s != "" {
                        out = append(out, s)
                }
        }
        return out
}

// ---------- workspace jail -------------------------------------------------

// InWorkspace verifies path stays inside root, following symlinks.
func InWorkspace(root, path string) error {
        absRoot, err := filepath.Abs(root)
        if err != nil {
                return err
        }
        absPath, err := filepath.Abs(path)
        if err != nil {
                return err
        }
        // resolve symlinks on the existing portion of the target path
        resolved := absPath
        if real, err := filepath.EvalSymlinks(absPath); err == nil {
                resolved = real
        } else if real, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
                resolved = filepath.Join(real, filepath.Base(absPath))
        }
        rel, err := filepath.Rel(absRoot, resolved)
        if err != nil {
                return fmt.Errorf("path escapes workspace")
        }
        if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
                return fmt.Errorf("path %q is outside the workspace %q", path, absRoot)
        }
        return nil
}

// DeniedPaths lists sensitive locations the agent may not read or write
// even when explicitly requested (defense against credential exfiltration).
var deniedSuffixes = []string{
        ".ssh/id_rsa", ".ssh/id_ed25519", ".ssh/id_ecdsa", ".ssh/authorized_keys",
        ".aws/credentials", ".netrc", ".gnupg", ".kube/config",
}

func IsDeniedPath(path string) (bool, string) {
        home, _ := os.UserHomeDir()
        home = filepath.Clean(home)
        p := filepath.Clean(path)
        for _, suffix := range deniedSuffixes {
                denied := filepath.Join(home, suffix)
                if strings.HasPrefix(p, denied) {
                        return true, "sensitive credential file"
                }
        }
        if filepath.Base(p) == ".env.production.local" {
                return false, "" // still readable, redaction handles it
        }
        return false, ""
}

// ---------- secret redaction ----------------------------------------------

var (
        reBearer      = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?bearer\s+)[A-Za-z0-9\-._~+/]+=*`)
        reAPIKeyJSON  = regexp.MustCompile(`(?i)("?(api[_-]?key|apikey|secret|token|password|access_token|refresh_token|client_secret)"?\s*[:=]\s*"?)([A-Za-z0-9\-._~+/]{8,})("?)`)
        reSkKeys      = regexp.MustCompile(`\b(sk|rk)-[A-Za-z0-9]{20,}\b`)
        reGitHub      = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)
        reAWS         = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
        reAnthropic   = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}\b`)
        reGoogle      = regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)
        rePEM         = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
        reSlackJWTish = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`)
        reEnvAssign   = regexp.MustCompile(`(?m)^([A-Z0-9_]*(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL)[A-Z0-9_]*)\s*=\s*(.+)$`)
)

// RedactString removes credential-looking material from arbitrary text.
func RedactString(s string) string {
        if s == "" {
                return s
        }
        s = rePEM.ReplaceAllString(s, "-----BEGIN PRIVATE KEY----- [redacted] -----END PRIVATE KEY-----")
        s = reBearer.ReplaceAllString(s, `${1}[redacted]`)
        s = reAPIKeyJSON.ReplaceAllString(s, `${1}[redacted]${4}`)
        s = reSkKeys.ReplaceAllString(s, "[redacted-key]")
        s = reGitHub.ReplaceAllString(s, "[redacted-github-token]")
        s = reAWS.ReplaceAllString(s, "[redacted-aws-key]")
        s = reAnthropic.ReplaceAllString(s, "[redacted-anthropic-key]")
        s = reGoogle.ReplaceAllString(s, "[redacted-google-key]")
        s = reSlackJWTish.ReplaceAllString(s, "[redacted-slack-token]")
        return s
}

// secretFileNames match files whose contents are credentials by convention.
var secretFileRes = []*regexp.Regexp{
        regexp.MustCompile(`(^|[\\/])\.env($|\.|([\\/]))`),
        regexp.MustCompile(`(?i)(^|[\\/])(credentials?|secrets?)(\.[a-z]+)?$`),
        regexp.MustCompile(`(?i)\.pem$`),
        regexp.MustCompile(`(?i)\.p12$`),
}

// RedactFileContent redacts file content before it is handed to the model
// when the file name looks like a credential store.
func RedactFileContent(path, content string) string {
        for _, re := range secretFileRes {
                if re.MatchString(path) {
                        lines := strings.Split(content, "\n")
                        for i, line := range lines {
                                trimmed := strings.TrimSpace(line)
                                if trimmed == "" || strings.HasPrefix(trimmed, "#") {
                                        continue
                                }
                                lines[i] = reEnvAssign.ReplaceAllString(line, "$1= [redacted by rup.ai]")
                                if strings.Contains(line, "PRIVATE KEY") {
                                        lines[i] = "[redacted by rup.ai]"
                                }
                        }
                        return strings.Join(lines, "\n")
                }
        }
        return content
}

// ---------- audit log ------------------------------------------------------

// AuditEntry records a security-relevant event.
type AuditEntry struct {
        Time      string          `json:"time"`
        Event     string          `json:"event"` // "command" | "write" | "delete" | "blocked" | "approval"
        Detail    json.RawMessage `json:"detail,omitempty"`
        Workspace string          `json:"workspace,omitempty"`
        Decision  string          `json:"decision,omitempty"` // approved | denied | blocked
}

// Audit appends an event to the user-level audit log (JSONL).
func Audit(event, workspace, decision string, detail any) {
        dir, err := DataDir()
        if err != nil {
                return
        }
        if err := os.MkdirAll(dir, 0o700); err != nil {
                return
        }
        f, err := os.OpenFile(filepath.Join(dir, "audit.log"),
                os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
        if err != nil {
                return
        }
        defer f.Close()
        entry := AuditEntry{Time: time.Now().UTC().Format(time.RFC3339), Event: event,
                Workspace: workspace, Decision: decision}
        if detail != nil {
                if raw, err := json.Marshal(detail); err == nil {
                        entry.Detail = raw
                }
        }
        raw, _ := json.Marshal(entry)
        _, _ = f.Write(append(raw, '\n'))
}

// ---------- helpers used by tools -----------------------------------------

// ShellForOS returns the shell invocation used to run commands.
func ShellForOS() (string, []string) {
        if os.Getenv("RUPAI_SHELL") != "" {
                return os.Getenv("RUPAI_SHELL"), []string{"-c"}
        }
        if isWindows() {
                comspec := os.Getenv("ComSpec")
                if comspec == "" {
                        comspec = "cmd.exe"
                }
                if strings.Contains(strings.ToLower(comspec), "powershell") || strings.Contains(strings.ToLower(comspec), "pwsh") {
                        return comspec, []string{"-NoLogo", "-Command"}
                }
                return comspec, []string{"/d", "/s", "/c"}
        }
        shell := os.Getenv("SHELL")
        if shell == "" {
                shell = "/bin/sh"
        }
        return shell, []string{"-c"}
}

func isWindows() bool { return runtime.GOOS == "windows" }

// SortPaths orders path strings for stable output.
func SortPaths(p []string) { sort.Strings(p) }
