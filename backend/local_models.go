package main

// local_models.go — local AI support: Ollama / llama.cpp / KoboldCpp
// detection and model discovery, pure-Go GGUF file parsing (metadata,
// quantization, size, RAM estimation), GGUF import/download, and an optional
// integrated runtime that launches a llama.cpp-compatible server binary
// (llama-server / llama-cli / llamafile) when one is installed on PATH.
//
// rup.ai does NOT bundle large model weights or llama.cpp binaries; cloud
// providers work without any local runtime (documented in BUILD.md).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ModelInfo describes a discovered local model.
type ModelInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	SizeMB   int64  `json:"size_mb"`
	Quant    string `json:"quant,omitempty"`
	Details  string `json:"details,omitempty"`
}

// GGUFInfo carries parsed GGUF metadata.
type GGUFInfo struct {
	Version    uint32
	TensorCnt  uint64
	KVCnt      uint64
	Arch       string
	Name       string
	Quant      string
	SizeBytes  int64
	CtxLength  uint64
	BlockCount uint64
}

// gguf KV type ids
const (
	ggufTypeUint8 = 0
	ggufTypeInt8  = 1
	ggufTypeUint16 = 2
	ggufTypeInt16  = 3
	ggufTypeUint32 = 4
	ggufTypeInt32  = 5
	ggufTypeFloat32 = 6
	ggufTypeBool    = 7
	ggufTypeString  = 8
	ggufTypeArray   = 9
	ggufTypeUint64  = 10
	ggufTypeInt64   = 11
	ggufTypeFloat64 = 12
)

// ParseGGUF reads GGUF header + metadata from a model file.
func ParseGGUF(path string) (*GGUFInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	info := &GGUFInfo{SizeBytes: st.Size(), Quant: "unknown"}

	r := bufio.NewReader(io.LimitReader(f, 64<<20)) // metadata can be large but bounded
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "GGUF" {
		return nil, errors.New("not a GGUF file (missing GGUF magic)")
	}
	if err := binary.Read(r, binary.LittleEndian, &info.Version); err != nil {
		return nil, fmt.Errorf("bad GGUF version: %w", err)
	}
	if info.Version < 2 {
		return nil, fmt.Errorf("unsupported GGUF version %d", info.Version)
	}
	if err := binary.Read(r, binary.LittleEndian, &info.TensorCnt); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &info.KVCnt); err != nil {
		return nil, err
	}
	for i := uint64(0); i < info.KVCnt; i++ {
		key, err := readGGUFString(r)
		if err != nil {
			return info, fmt.Errorf("metadata truncated: %w", err)
		}
		typ, err := readGGUFUint32(r)
		if err != nil {
			return info, err
		}
		val, err := readGGUFValue(r, int(typ))
		if err != nil {
			return info, err
		}
		switch key {
		case "general.architecture":
			info.Arch, _ = val.(string)
		case "general.name":
			info.Name, _ = val.(string)
		case string("general.file_type"):
			if n, ok := val.(uint32); ok {
				info.Quant = ggufQuantName(n)
			}
		case "llama.context_length", "context_length", string("."+info.Arch+".context_length"):
			if n, ok := val.(uint64); ok {
				info.CtxLength = n
			}
		case "llama.block_count", string("."+info.Arch+".block_count"):
			if n, ok := val.(uint64); ok {
				info.BlockCount = n
			}
		}
	}
	if q := inferQuantFromName(filepath.Base(path)); info.Quant == "unknown" && q != "" {
		info.Quant = q
	}
	return info, nil
}

func readGGUFString(r *bufio.Reader) (string, error) {
	n, err := readGGUFUint64(r)
	if err != nil {
		return "", err
	}
	if n > 64<<20 {
		return "", fmt.Errorf("gguf string too long (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readGGUFUint32(r *bufio.Reader) (uint32, error) {
	var v uint32
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func readGGUFUint64(r *bufio.Reader) (uint64, error) {
	var v uint64
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

// readGGUFValue consumes one value of the given type; arrays are skipped
// efficiently (only element sizes are read).
func readGGUFValue(r *bufio.Reader, typ int) (any, error) {
	switch typ {
	case ggufTypeUint8, ggufTypeInt8, ggufTypeBool:
		b, err := r.ReadByte()
		return b, err
	case ggufTypeUint16, ggufTypeInt16:
		var v int16
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return v, nil
	case ggufTypeUint32, ggufTypeInt32, ggufTypeFloat32:
		var v int32
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return v, nil
	case ggufTypeUint64, ggufTypeInt64, ggufTypeFloat64:
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, err
		}
		return v, nil
	case ggufTypeString:
		return readGGUFString(r)
	case ggufTypeArray:
		elemType, err := readGGUFUint32(r)
		if err != nil {
			return nil, err
		}
		count, err := readGGUFUint64(r)
		if err != nil {
			return nil, err
		}
		elemSize := ggufElemSize(int(elemType))
		if elemSize == -1 {
			// string array: must read each string (bounded)
			for i := uint64(0); i < count; i++ {
				if _, err := readGGUFString(r); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
		if _, err := io.CopyN(io.Discard, r, int64(elemSize)*int64(count)); err != nil {
			return nil, fmt.Errorf("gguf array truncated: %w", err)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown gguf value type %d", typ)
}

func ggufElemSize(typ int) int {
	switch typ {
	case ggufTypeUint8, ggufTypeInt8, ggufTypeBool:
		return 1
	case ggufTypeUint16, ggufTypeInt16:
		return 2
	case ggufTypeUint32, ggufTypeInt32, ggufTypeFloat32:
		return 4
	case ggufTypeUint64, ggufTypeInt64, ggufTypeFloat64:
		return 8
	case ggufTypeString:
		return -1
	}
	return -1
}

// ggufQuantName maps general.file_type ids to friendly quant names.
func ggufQuantName(n uint32) string {
	switch n {
	case 0:
		return "F32"
	case 1:
		return "F16"
	case 2:
		return "Q4_0"
	case 3:
		return "Q4_1"
	case 7:
		return "Q8_0"
	case 8:
		return "Q5_0"
	case 9:
		return "Q5_1"
	case 10:
		return "Q2_K"
	case 11:
		return "Q3_K_S"
	case 12:
		return "Q3_K_M"
	case 13:
		return "Q3_K_L"
	case 14:
		return "Q4_K_S"
	case 15:
		return "Q4_K_M"
	case 16:
		return "Q5_K_S"
	case 17:
		return "Q5_K_M"
	case 18:
		return "Q6_K"
	case 19:
		return "IQ2_XXS"
	case 20:
		return "IQ2_XS"
	case 21:
		return "Q2_K_S"
	case 22:
		return "IQ3_XS"
	case 23:
		return "IQ3_XXS"
	case 24:
		return "IQ1_S"
	case 25:
		return "IQ4_NL"
	case 26:
		return "IQ3_S"
	case 27:
		return "IQ3_M"
	case 28:
		return "IQ2_S"
	case 29:
		return "IQ2_M"
	case 30:
		return "IQ4_XS"
	case 31:
		return "IQ1_M"
	case 32:
		return "BF16"
	case 36:
		return "TQ1_0"
	case 37:
		return "TQ2_0"
	case 38:
		return "MXFP4"
	}
	return "type-" + strconv.FormatUint(uint64(n), 10)
}

func inferQuantFromName(name string) string {
	upper := strings.ToUpper(name)
	for _, q := range []string{"Q8_0", "Q6_K", "Q5_K_M", "Q5_K_S", "Q5_1", "Q5_0", "Q4_K_M", "Q4_K_S", "Q4_1", "Q4_0", "Q3_K_L", "Q3_K_M", "Q3_K_S", "Q2_K", "IQ4_XS", "IQ3_M", "IQ2_M", "IQ1_S", "F16", "BF16", "F32", "MXFP4"} {
		if strings.Contains(upper, q) {
			return q
		}
	}
	return ""
}

// EstimateRAM returns a rough runtime memory estimate for a GGUF file.
// Rule of thumb: weights + ~15% overhead + KV cache guess. Documented as an
// estimate, not a guarantee.
func EstimateRAM(g *GGUFInfo) int64 {
	base := g.SizeBytes + g.SizeBytes/7
	kv := int64(0)
	if g.CtxLength > 0 && g.BlockCount > 0 {
		kv = int64(g.CtxLength) * int64(g.BlockCount) * 64 // very rough
		if kv > 4<<30 {
			kv = 4 << 30
		}
	}
	return base + kv
}

// SystemRAMBytes returns total physical memory using per-OS strategies that
// compile on every platform (no build tags in rup.ai source).
func SystemRAMBytes() (uint64, error) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, err := strconv.ParseUint(fields[1], 10, 64)
					if err == nil {
						return kb * 1024, nil
					}
				}
			}
		}
		return 0, errors.New("MemTotal not found in /proc/meminfo")
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	default: // windows and others: shell out to PowerShell/CMD
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory").Output()
		if err != nil || len(out) == 0 {
			return 0, fmt.Errorf("could not determine system memory: %v", err)
		}
		return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	}
}

// ---------- provider detection --------------------------------------------

// DetectOllama lists models from an Ollama server's native API.
func DetectOllama(ctx context.Context, baseURL string) ([]ModelInfo, error) {
	url := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	url += "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama not reachable at %s", url)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var wire struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			Details    struct {
				QuantizationLevel string `json:"quantization_level"`
				Family            string `json:"family"`
				ParameterSize     string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("unexpected ollama response")
	}
	out := make([]ModelInfo, 0, len(wire.Models))
	for _, m := range wire.Models {
		out = append(out, ModelInfo{Name: m.Name, Provider: "ollama",
			SizeMB: m.Size / (1024 * 1024), Quant: m.Details.QuantizationLevel,
			Details: strings.TrimSpace(m.Details.ParameterSize + " " + m.Details.Family)})
	}
	return out, nil
}

// DetectKobold asks KoboldCpp for its current model.
func DetectKobold(ctx context.Context, baseURL string) ([]ModelInfo, error) {
	url := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1") + "/api/v1/model"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("koboldcpp not reachable at %s", url)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var wire struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(raw, &wire)
	name := wire.Result
	if name == "" {
		name = "koboldcpp"
	}
	return []ModelInfo{{Name: name, Provider: "koboldcpp"}}, nil
}

// ---------- GGUF management -------------------------------------------------

// ModelsDir returns the local model directory.
func ModelsDir() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "models")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// ImportGGUF copies a GGUF file into the models directory.
func ImportGGUF(src, destName string) (string, error) {
	dir, err := ModelsDir()
	if err != nil {
		return "", err
	}
	if destName == "" {
		destName = filepath.Base(src)
	}
	if !strings.HasSuffix(strings.ToLower(destName), ".gguf") {
		destName += ".gguf"
	}
	dest := filepath.Join(dir, filepath.Base(destName))
	if abs, err := filepath.Abs(src); err == nil {
		if _, err := ParseGGUF(abs); err != nil {
			return "", fmt.Errorf("source is not a valid GGUF file: %v", err)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return dest, nil
}

// DownloadGGUF streams a model file from a user-provided URL with progress.
// The user chooses the source; rup.ai never downloads models automatically.
func DownloadGGUF(ctx context.Context, url, destName string, progress func(down, total int64)) (string, error) {
	dir, err := ModelsDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filepath.Base(destName))
	if !strings.HasSuffix(strings.ToLower(dest), ".gguf") {
		dest += ".gguf"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: status %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(tmp)
				return "", werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return "", err
		}
	}
	out.Close()
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ---------- integrated llama.cpp-based runtime ------------------------------

// ManagedProcess tracks a locally launched inference server.
type ManagedProcess struct {
	Cmd     *exec.Cmd
	BaseURL string
	Model   string
	Started time.Time
}

var managedProc *ManagedProcess

// FindLlamaRuntime looks for a llama.cpp-compatible server binary on PATH.
func FindLlamaRuntime() (string, error) {
	candidates := []string{"llama-server", "llamafile", "llama-cli"}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no llama.cpp runtime found on PATH (install llama.cpp and ensure `llama-server` is available)")
}

// StartGGUFServer launches a GGUF model on a llama.cpp-compatible runtime.
// CPU inference by default; GPU layers optional (-ngl).
func StartGGUFServer(ctx context.Context, ggufPath string, ctxLen, threads, gpuLayers, port int) (*ManagedProcess, error) {
	bin, err := FindLlamaRuntime()
	if err != nil {
		return nil, err
	}
	info, err := ParseGGUF(ggufPath)
	if err != nil {
		return nil, err
	}
	ram, ramErr := SystemRAMBytes()
	if ramErr == nil {
		need := uint64(EstimateRAM(info))
		if need > ram {
			return nil, fmt.Errorf("this model needs roughly %s RAM but this machine reports %s — pick a smaller quantization or a smaller model",
				humanBytes(need), humanBytes(ram))
		}
	}
	if port == 0 {
		port = 8081
	}
	if ctxLen <= 0 {
		ctxLen = 4096
	}
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	args := []string{"-m", ggufPath, "-c", strconv.Itoa(ctxLen), "-t", strconv.Itoa(threads),
		"--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	if gpuLayers > 0 {
		args = append(args, "-ngl", strconv.Itoa(gpuLayers))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %v", bin, err)
	}
	mp := &ManagedProcess{Cmd: cmd, BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Model: filepath.Base(ggufPath), Started: time.Now()}
	managedProc = mp
	go func() { _ = cmd.Wait() }()
	return mp, nil
}

// StopGGUFServer terminates the managed runtime, if any.
func StopGGUFServer() bool {
	if managedProc == nil || managedProc.Cmd.Process == nil {
		return false
	}
	_ = managedProc.Cmd.Process.Kill()
	managedProc = nil
	return true
}

// CheckLocalServer probes an OpenAI-compatible local endpoint.
func CheckLocalServer(ctx context.Context, baseURL string) error {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("no server at %s", baseURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server at %s returned %d", baseURL, resp.StatusCode)
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, e := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		e++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[e+1])
}

// localPresetsForWizard returns wizard-ready entries for local providers.
func localPresetsForWizard() []Preset {
	var out []Preset
	for _, p := range Presets {
		if p.Local {
			out = append(out, p)
		}
	}
	return out
}

var _ = bytes.MinRead
