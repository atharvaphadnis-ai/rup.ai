package main

// main.go — process entry point: flag parsing and dispatch to cli.Run.
//
// rup.ai — terminal-first AI coding agent.

import (
        "fmt"
        "os"
        "strings"
)

// version is overridden at build time with:
//
//      go build -ldflags "-X main.version=v1.2.3" ./backend
var version = "0.1.2"

func main() {
        args := normalizeFlags(os.Args[1:])
        applyFlagEnv(args)
        os.Exit(Run(args))
}

// normalizeFlags extracts global flags into environment variables so the rest
// of the program reads configuration uniformly.
func normalizeFlags(args []string) []string {
        var out []string
        for i := 0; i < len(args); i++ {
                a := args[i]
                switch {
                case a == "--cwd" || a == "-C":
                        if i+1 < len(args) {
                                _ = os.Setenv("RUPAI_CWD", args[i+1])
                                i++
                        }
                case strings.HasPrefix(a, "--cwd="):
                        _ = os.Setenv("RUPAI_CWD", strings.TrimPrefix(a, "--cwd="))
                case a == "--provider" || a == "-p":
                        if i+1 < len(args) {
                                _ = os.Setenv("RUPAI_PROVIDER", args[i+1])
                                i++
                        }
                case strings.HasPrefix(a, "--provider="):
                        _ = os.Setenv("RUPAI_PROVIDER", strings.TrimPrefix(a, "--provider="))
                case a == "--model" || a == "-m":
                        if i+1 < len(args) {
                                _ = os.Setenv("RUPAI_MODEL", args[i+1])
                                i++
                        }
                case strings.HasPrefix(a, "--model="):
                        _ = os.Setenv("RUPAI_MODEL", strings.TrimPrefix(a, "--model="))
                case a == "--mode":
                        if i+1 < len(args) {
                                _ = os.Setenv("RUPAI_MODE", args[i+1])
                                i++
                        }
                case strings.HasPrefix(a, "--mode="):
                        _ = os.Setenv("RUPAI_MODE", strings.TrimPrefix(a, "--mode="))
                case a == "--no-anim":
                        _ = os.Setenv("RUPAI_NO_ANIM", "1")
                case a == "--plain":
                        _ = os.Setenv("RUPAI_PLAIN", "1")
                case a == "--approve-all":
                        // documented escape hatch for CI; still blocks catastrophic commands
                        _ = os.Setenv("RUPAI_APPROVAL", "permissive")
                default:
                        out = append(out, a)
                }
        }
        return out
}

// applyFlagEnv prints an early hint when a workspace flag is bogus.
func applyFlagEnv(args []string) {
        if cwd := os.Getenv("RUPAI_CWD"); cwd != "" {
                if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
                        fmt.Fprintf(os.Stderr, "rup.ai: workspace %q is not a directory\n", cwd)
                        os.Exit(2)
                }
        }
}
