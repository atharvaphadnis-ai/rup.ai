package main

// terminal.go — the terminal user interface: the animated rup.ai banner,
// colors, spinner, streaming output rendering, prompts and tables.
//
// All output is routed through go-colorable: on Windows consoles it first
// enables native VT processing, and on consoles that predate VT support it
// TRANSLATES every escape sequence into native console API calls — so colors
// and the animation always render, never print as literal "←[38;5;…" codes.
// It degrades further and gracefully: non-TTY sessions, TERM=dumb, NO_COLOR
// and RUPAI_PLAIN=1 all disable animation and color entirely (zero escape
// bytes in the output) while keeping every feature usable.

import (
        "bufio"
        "fmt"
        "io"
        "os"
        "strconv"
        "strings"
        "sync"
        "time"

        "golang.org/x/term"

        "github.com/mattn/go-colorable"
)

// ANSI color codes (256-color palette, broadly supported).
const (
        ansiReset  = "\x1b[0m"
        ansiDim    = "\x1b[2m"
        ansiBold   = "\x1b[1m"
        ansiCyan   = "\x1b[38;5;51m"
        ansiBlue   = "\x1b[38;5;39m"
        ansiViolet = "\x1b[38;5;141m"
        ansiGreen  = "\x1b[38;5;114m"
        ansiYellow = "\x1b[38;5;179m"
        ansiRed    = "\x1b[38;5;203m"
        ansiGray   = "\x1b[38;5;245m"
        ansiText   = "\x1b[38;5;250m"
)

// Terminal wraps stdout with styling and animation helpers.
type Terminal struct {
        Out    io.Writer
        Plain  bool
        isTTY  bool
        Width  int
        mu     sync.Mutex
        stream int // bytes streamed in the current model turn
}

// NewTerminal builds the UI; plain mode can be forced via config or env.
func NewTerminal(cfg *Config) *Terminal {
        t := &Terminal{Out: os.Stdout}
        // Windows: enable native VT processing when the console supports it
        // (Windows Terminal, Win10 1607+ conhost), then wrap stdout so that on
        // consoles WITHOUT VT support every escape sequence is translated
        // into native console calls instead of printed literally.
        _ = colorable.EnableColorsStdout(nil)
        t.Out = colorable.NewColorableStdout()
        t.isTTY = term.IsTerminal(int(os.Stdout.Fd()))
        if cfg != nil && cfg.UI.Plain {
                t.Plain = true
        }
        if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
                t.Plain = true
        }
        if !t.isTTY {
                t.Plain = true
        }
        w := 80
        if t.isTTY {
                if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 20 {
                        w = width
                }
        }
        t.Width = w
        return t
}

// c wraps a string in a color unless plain.
func (t *Terminal) c(code, s string) string {
        if t.Plain {
                return s
        }
        return code + s + ansiReset
}

func (t *Terminal) print(a ...any) {
        t.mu.Lock()
        defer t.mu.Unlock()
        fmt.Fprint(t.Out, a...)
}

func (t *Terminal) printf(format string, a ...any) {
        t.mu.Lock()
        defer t.mu.Unlock()
        fmt.Fprintf(t.Out, format, a...)
}

// Printf exposes formatted printing to other modules.
func (t *Terminal) Printf(format string, a ...any) { t.printf(format, a...) }

// Println prints a raw line.
func (t *Terminal) Println(a ...any) { t.print(append(a, "\n")...) }

// sleep pauses the animation (no-op in plain mode).
func (t *Terminal) sleep(d time.Duration) {
        if t.Plain {
                return
        }
        time.Sleep(d)
}

// --------------------------------------------------------------------------
// Banner + the rup.ai reveal animation
// --------------------------------------------------------------------------

// logoGlyphs are the 5-row block letters that compose the big "rup.ai"
// wordmark (single-width cells, so column-based gradient math is exact).
var logoGlyphs = map[byte][]string{
        'r': {"███ ", "██ █", "███ ", "██  ", "██  "},
        'u': {"     ", "██ ██", "██ ██", "██ ██", " ███ "},
        'p': {"███  ", "██ ██", "███  ", "██   ", "██   "},
        '.': {"  ", "  ", "  ", "  ", "██"},
        'a': {"     ", " ███ ", "██ ██", "██ ██", " ███ "},
        'i': {"██", "  ", "██", "██", "██"},
}

// gradPalette sweeps cyan → azure → violet → pink across the wordmark.
var gradPalette = []string{
        "\x1b[38;5;51m", "\x1b[38;5;45m", "\x1b[38;5;39m", "\x1b[38;5;38m",
        "\x1b[38;5;99m", "\x1b[38;5;134m", "\x1b[38;5;141m", "\x1b[38;5;177m",
}

// composeLogo renders the big wordmark as 5 lines of equal width. Building
// it at runtime from per-letter cells guarantees perfect alignment.
func composeLogo() []string {
        const word = "rup.ai"
        rows := make([]string, 5)
        for r := range rows {
                var b strings.Builder
                for i, ch := range []byte(word) {
                        g, ok := logoGlyphs[ch]
                        if !ok {
                                continue
                        }
                        if i > 0 {
                                b.WriteString(" ")
                        }
                        b.WriteString(g[r])
                }
                rows[r] = b.String()
        }
        return rows
}

// gradient colors one line with the horizontal wordmark sweep; col is the
// line's first on-screen column, width the art's full width. No-op in
// plain mode — it must never inject escape codes there.
func (t *Terminal) gradient(s string, col, width int) string {
        if t.Plain || width <= 0 {
                return s
        }
        var b strings.Builder
        last := -1
        for _, ch := range s {
                idx := col * len(gradPalette) / width
                if idx >= len(gradPalette) {
                        idx = len(gradPalette) - 1
                }
                if idx != last {
                        b.WriteString(gradPalette[idx])
                        last = idx
                }
                b.WriteRune(ch)
                col++
        }
        b.WriteString(ansiReset)
        return b.String()
}

// Banner prints the rup.ai startup banner. With animation enabled it plays
// the signature reveal: the paper plane flies in along a dotted path, the
// big "rup.ai" wordmark materializes row by row with a horizontal
// cyan→violet gradient, then the tagline types out beneath it.
func (t *Terminal) Banner(animate bool) {
        const (
                dot     = "·"
                plane   = ">"
                word    = "rup.ai"
                tagline = "terminal-first AI coding agent"
        )

        // RUPAI_FORCE_ANIM=1 forces the animation for demos/capture even on pipes.
        if os.Getenv("RUPAI_FORCE_ANIM") == "1" {
                animate = true
                t.Plain = false
        }

        logo := composeLogo()
        logoWidth := 0
        if len(logo) > 0 {
                logoWidth = len([]rune(logo[0]))
        }
        bigEnough := t.Width >= logoWidth+6

        // drawSmall is the narrow-terminal fallback.
        drawSmall := func() {
                t.print("\n")
                t.print("  " + plane + strings.Repeat(" "+dot, 10) + "\n\n")
                t.print("  " + t.c(ansiBold+ansiCyan, word) + "\n")
                t.print("  " + t.c(ansiGray, tagline) + "\n")
                t.print("  " + t.c(ansiGray, strings.Repeat("─", 38)) + "\n\n")
        }

        // drawBig renders the full banner; animate adds the motion.
        drawBig := func(animate bool) {
                t.print("\n")
                if animate && !t.Plain {
                        // the plane flies in along the dotted path, in place
                        const steps = 12
                        for i := 0; i <= steps; i++ {
                                line := "   "
                                for j := 0; j < steps; j++ {
                                        switch {
                                        case j < i:
                                                line += " " + dot
                                        case j == i:
                                                line += " " + t.c(ansiCyan+ansiBold, plane)
                                        default:
                                                line += "  "
                                        }
                                }
                                t.print("\r" + line)
                                t.sleep(34 * time.Millisecond)
                        }
                        parked := "   " + strings.Repeat(" "+dot, steps) + " " + t.c(ansiCyan+ansiBold, plane)
                        t.print("\r" + parked + "\n")
                } else {
                        t.print("  " + plane + strings.Repeat(" "+dot, 12) + "\n")
                }
                t.sleep(140 * time.Millisecond)
                t.print("\n")

                // the big wordmark materializes row by row
                for _, row := range logo {
                        t.print("   " + t.gradient(row, 3, logoWidth) + "\n")
                        if animate {
                                t.sleep(85 * time.Millisecond)
                        }
                }
                t.sleep(120 * time.Millisecond)
                t.print("\n")

                // tagline types out beneath the wordmark
                if animate && !t.Plain {
                        t.print("   ")
                        for _, ch := range tagline {
                                t.print(t.c(ansiGray, string(ch)))
                                t.sleep(9 * time.Millisecond)
                        }
                        t.print("\n")
                } else {
                        t.print("   " + t.c(ansiGray, tagline) + "\n")
                }
                t.print("   " + t.gradient(strings.Repeat("─", min(46, t.Width-6)), 3, logoWidth) + "\n")
                t.sleep(100 * time.Millisecond)
                t.print("\n")
        }

        if !bigEnough {
                drawSmall()
                return
        }
        drawBig(animate && !t.Plain)
}

func min(a, b int) int {
        if a < b {
                return a
        }
        return b
}

// --------------------------------------------------------------------------
// Status line & sections
// --------------------------------------------------------------------------

// StatusLine renders the session header (provider/model/mode/workspace).
func (t *Terminal) StatusLine(provider, model, mode, workspace string) {
        t.printf("%s%s %s%s %s%s %s%s\n",
                t.c(ansiGray, "  provider: "), t.c(ansiCyan, provider),
                t.c(ansiGray, "  model: "), t.c(ansiViolet, model),
                t.c(ansiGray, "  mode: "), t.c(ansiBlue, mode),
                t.c(ansiGray, "  workspace: "), t.c(ansiText, workspace))
        t.print("\n")
}

// Section prints a titled section divider.
func (t *Terminal) Section(title string) {
        t.print("\n" + t.c(ansiBold, title) + "\n" + t.c(ansiGray, strings.Repeat("─", min(50, t.Width-2))) + "\n")
}

// Block prints an indented titled block.
func (t *Terminal) Block(title, body string) {
        t.print(t.c(ansiGray, title+": ") + "\n")
        for _, line := range strings.Split(body, "\n") {
                t.print("  " + line + "\n")
        }
}

// Info prints an informational line.
func (t *Terminal) Info(msg string)  { t.print(t.c(ansiGray, "  · "+msg) + "\n") }
// Success prints a green confirmation.
func (t *Terminal) Success(msg string) { t.print(t.c(ansiGreen, "  ✔ "+msg) + "\n") }
// Warn prints a yellow warning.
func (t *Terminal) Warn(msg string)  { t.print(t.c(ansiYellow, "  ! "+msg) + "\n") }
// Error prints a red error.
func (t *Terminal) Error(msg string) { t.print(t.c(ansiRed, "  ✗ "+msg) + "\n") }

// --------------------------------------------------------------------------
// Spinner
// --------------------------------------------------------------------------

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner starts an animated status spinner; the returned func stops it.
func (t *Terminal) Spinner(text string) func() {
        t.mu.Lock()
        if t.Plain {
                t.mu.Unlock()
                t.print(t.c(ansiGray, "  · "+text+"…") + "\n")
                return func() {}
        }
        t.mu.Unlock()
        done := make(chan struct{})
        var wg sync.WaitGroup
        wg.Add(1)
        go func() {
                defer wg.Done()
                i := 0
                for {
                        select {
                        case <-done:
                                return
                        default:
                        }
                        frame := t.c(ansiCyan, spinnerFrames[i%len(spinnerFrames)])
                        t.printf("\r  %s %s  ", frame, t.c(ansiGray, text))
                        i++
                        time.Sleep(80 * time.Millisecond)
                }
        }()
        stop := func() {
                close(done)
                wg.Wait()
                // clear the spinner line with a \r + spaces overwrite: this
                // renders correctly on every console ever made, no escape
                // sequences required
                t.printf("\r%s\r", strings.Repeat(" ", len([]rune(text))+6))
        }
        return stop
}

// --------------------------------------------------------------------------
// Streaming & model output
// --------------------------------------------------------------------------

// StreamDelta prints a streaming chunk from the model.
func (t *Terminal) StreamDelta(delta string) {
        t.mu.Lock()
        t.stream += len(delta)
        t.mu.Unlock()
        t.print(delta)
}

// ModelText prints a complete model message (skipped when it was streamed).
func (t *Terminal) ModelText(content string) {
        t.mu.Lock()
        streamed := t.stream
        t.stream = 0
        t.mu.Unlock()
        if streamed > 0 {
                if !strings.HasSuffix(content, "\n") {
                        t.print("\n")
                }
                return
        }
        for _, line := range strings.Split(content, "\n") {
                t.print(t.renderInline(line) + "\n")
        }
}

// renderInline applies minimal markdown-ish styling to one line
// (no-op in plain mode — it must never inject escape codes there).
func (t *Terminal) renderInline(line string) string {
        // dim indented blockquote-ish lines, highlight inline code spans
        if strings.HasPrefix(strings.TrimSpace(line), "```") {
                return t.c(ansiGray, line)
        }
        out := line
        if t0 := strings.Count(out, "`"); t0 >= 2 {
                parts := strings.Split(out, "`")
                for i := 1; i < len(parts)-1; i += 2 {
                        parts[i] = t.c(ansiCyan, parts[i])
                }
                out = strings.Join(parts, "`")
        }
        return out
}

// ToolStart announces a tool call.
func (t *Terminal) ToolStart(name, argsJSON string) {
        preview := argsJSON
        preview = strings.NewReplacer("\n", " ", "\r", "").Replace(preview)
        if len(preview) > 90 {
                preview = preview[:90] + "…"
        }
        t.printf("\n%s %s %s\n", t.c(ansiViolet, "› tool"), t.c(ansiBold, name), t.c(ansiGray, preview))
}

// ToolDone reports a completed tool call.
func (t *Terminal) ToolDone(name string, d time.Duration, ok bool) {
        dur := strconv.FormatInt(d.Milliseconds(), 10) + "ms"
        if ok {
                t.print(t.c(ansiGreen, "  ✔ "+name+" ("+dur+")") + "\n")
        } else {
                t.print(t.c(ansiRed, "  ✗ "+name+" ("+dur+")") + "\n")
        }
}

// RenderDiff prints a unified diff with color.
func (t *Terminal) RenderDiff(diff string) {
        for _, line := range strings.Split(diff, "\n") {
                switch {
                case strings.HasPrefix(line, "+"):
                        t.print(t.c(ansiGreen, line) + "\n")
                case strings.HasPrefix(line, "-"):
                        t.print(t.c(ansiRed, line) + "\n")
                case strings.HasPrefix(line, "@@"):
                        t.print(t.c(ansiBlue, line) + "\n")
                default:
                        t.print(t.c(ansiGray, line) + "\n")
                }
        }
}

// Table renders a simple aligned table.
func (t *Terminal) Table(headers []string, rows [][]string) {
        widths := make([]int, len(headers))
        for i, h := range headers {
                widths[i] = len(h)
        }
        for _, row := range rows {
                for i, cell := range row {
                        if i < len(widths) && len(cell) > widths[i] {
                                widths[i] = len(cell)
                        }
                }
        }
        var line strings.Builder
        for i, h := range headers {
                line.WriteString(pad(h, widths[i]) + "  ")
        }
        t.print(t.c(ansiGray, strings.TrimRight(line.String(), " ")) + "\n")
        for _, row := range rows {
                var rl strings.Builder
                for i := range headers {
                        cell := ""
                        if i < len(row) {
                                cell = row[i]
                        }
                        rl.WriteString(pad(cell, widths[i]) + "  ")
                }
                t.print(strings.TrimRight(rl.String(), " ") + "\n")
        }
}

func pad(s string, w int) string {
        for len(s) < w {
                s += " "
        }
        return s
}

// --------------------------------------------------------------------------
// Input
// --------------------------------------------------------------------------

var stdinReader = bufio.NewReader(os.Stdin)

// ReadLine reads one line of input with a prompt.
func (t *Terminal) ReadLine(prompt string) (string, error) {
        t.print(t.c(ansiCyan, prompt) + " ")
        line, err := stdinReader.ReadString('\n')
        if err != nil && line == "" {
                return "", err
        }
        return strings.TrimRight(line, "\r\n"), nil
}

// Password reads a secret without echo.
func (t *Terminal) Password(prompt string) (string, error) {
        t.print(t.c(ansiCyan, prompt) + " ")
        if !t.isTTY {
                line, err := stdinReader.ReadString('\n')
                if err != nil && line == "" {
                        return "", err
                }
                return strings.TrimRight(line, "\r\n"), nil
        }
        raw, err := term.ReadPassword(int(os.Stdin.Fd()))
        t.print("\n")
        if err != nil {
                return "", err
        }
        return string(raw), nil
}

// Choice prints numbered options and returns the 1-based selection.
func (t *Terminal) Choice(prompt string, options []string, def int) int {
        t.print("\n")
        for i, opt := range options {
                t.printf("  %s %s\n", t.c(ansiViolet, "["+strconv.Itoa(i+1)+"]"), opt)
        }
        for {
                answer, err := t.ReadLine(prompt)
                if err != nil {
                        return def
                }
                answer = strings.TrimSpace(answer)
                if answer == "" {
                        return def
                }
                if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(options) {
                        return n
                }
                t.Warn("enter a number between 1 and " + strconv.Itoa(len(options)))
        }
}

// Confirm asks a yes/no question; empty answer defaults to def.
func (t *Terminal) Confirm(prompt string, def bool) bool {
        suffix := " [y/N] "
        if def {
                suffix = " [Y/n] "
        }
        answer, err := t.ReadLine(prompt + suffix)
        if err != nil {
                return def
        }
        answer = strings.ToLower(strings.TrimSpace(answer))
        if answer == "" {
                return def
        }
        return answer == "y" || answer == "yes"
}

// EofDetected reports whether stdin reached EOF (used to exit REPL cleanly).
func (t *Terminal) EofDetected(err error) bool {
        return err == io.EOF
}

// Pause briefly (used between animation phases).
func (t *Terminal) Pause(d time.Duration) { t.sleep(d) }
