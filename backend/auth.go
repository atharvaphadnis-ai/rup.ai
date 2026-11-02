package main

// auth.go — credential storage and authentication flows.
//
// Credentials are stored in the OS secure credential store (Windows
// Credential Manager, macOS Keychain, freedesktop Secret Service / keyring)
// via github.com/zalando/go-keyring. When no keyring is available (e.g.
// headless Linux without dbus), a 0600-permission file under the config dir
// is used and the limitation is reported to the user.
//
// IMPORTANT (honesty policy, mirrors README): subscription-based login for
// OpenAI (ChatGPT) and Anthropic (Claude Pro/Max) is restricted by those
// companies to their own first-party clients. rup.ai therefore implements:
//   1. API-key authentication for every cloud provider (officially supported).
//   2. A generic OAuth2 client (device flow + authorization-code + PKCE) that
//      is used when a provider publishes official third-party OAuth endpoints
//      (configurable per provider, e.g. self-hosted gateways).
// It never impersonates official clients, never extracts browser cookies and
// never scrapes private endpoints.

import (
        "context"
        "crypto/rand"
        "crypto/sha256"
        "encoding/base64"
        "encoding/json"
        "errors"
        "fmt"
        "html"
        "io"
        "net"
        "net/http"
        "net/url"
        "os"
        "path/filepath"
        "strings"
        "time"

        "github.com/zalando/go-keyring"
)

const secretService = "rup.ai"
const secretUser = "default"

// credentialFile is the fallback store: JSON, 0600, outside any repo.
type credentialFile map[string]map[string]string

func credsPath() (string, error) {
        dir, err := ConfigDir()
        if err != nil {
                return "", err
        }
        return filepath.Join(dir, "credentials.json"), nil
}

func readCreds() credentialFile {
        out := credentialFile{}
        path, err := credsPath()
        if err != nil {
                return out
        }
        raw, err := os.ReadFile(path)
        if err != nil {
                return out
        }
        _ = json.Unmarshal(raw, &out)
        return out
}

func writeCreds(c credentialFile) error {
        path, err := credsPath()
        if err != nil {
                return err
        }
        if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
                return err
        }
        data, _ := json.MarshalIndent(c, "", "  ")
        return os.WriteFile(path, data, 0o600)
}

// StoreSecret stores provider credential "name" (e.g. "api_key").
func StoreSecret(providerID, name, value string) error {
        if value == "" {
                return nil
        }
        if os.Getenv("RUPAI_STORE") == "file" {
                return storeSecretFile(providerID, name, value)
        }
        err := keyring.Set(secretService, providerID+":"+name, value)
        if err == nil {
                return nil
        }
        if errors.Is(err, keyring.ErrUnsupportedPlatform) || errors.Is(err, keyring.ErrNotFound) || err != nil {
                // any keyring failure falls back to the file store
                return storeSecretFile(providerID, name, value)
        }
        return nil
}

func storeSecretFile(providerID, name, value string) error {
        creds := readCreds()
        if creds[providerID] == nil {
                creds[providerID] = map[string]string{}
        }
        creds[providerID][name] = value
        return writeCreds(creds)
}

// LoadSecret reads a stored credential, preferring the keyring.
func LoadSecret(providerID, name string) string {
        if os.Getenv("RUPAI_STORE") != "file" {
                if v, err := keyring.Get(secretService, providerID+":"+name); err == nil && v != "" {
                        return v
                }
        }
        creds := readCreds()
        if m, ok := creds[providerID]; ok {
                return m[name]
        }
        return ""
}

// DeleteSecret removes a credential from both stores.
func DeleteSecret(providerID, name string) error {
        var firstErr error
        if err := keyring.Delete(secretService, providerID+":"+name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
                firstErr = err
        }
        creds := readCreds()
        if m, ok := creds[providerID]; ok {
                delete(m, name)
                if len(m) == 0 {
                        delete(creds, providerID)
                }
                if err := writeCreds(creds); err != nil && firstErr == nil {
                        firstErr = err
                }
        }
        return firstErr
}

// APIKey resolves the API key for a provider: env override first, then the
// secret store. Environment names: RUPAI_API_KEY or RUPAI_<PROVIDER>_API_KEY.
func APIKey(providerID string) string {
        if v := os.Getenv("RUPAI_API_KEY"); v != "" {
                return v
        }
        upper := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(providerID))
        if v := os.Getenv("RUPAI_" + upper + "_API_KEY"); v != "" {
                return v
        }
        return LoadSecret(providerID, "api_key")
}

// LoginFlow runs the interactive login for a provider and stores credentials.
func LoginFlow(cfg *Config, tui *Terminal, providerID string) error {
        pcfg := cfg.EnsureProvider(providerID, "")
        preset, known := FindPreset(providerID)

        tui.Section("Sign in — " + providerID)
        if known && preset.AuthNote != "" {
                tui.Block("Access model", preset.AuthNote)
        }
        tui.Block("Access types",
                "[1] API-key authentication  (officially supported by all cloud providers)\n"+
                        "[2] OAuth2 sign-in          (only where a provider publishes third-party OAuth)\n"+
                        "[3] No authentication       (local servers such as Ollama)")
        switch strings.ToLower(pcfg.Auth) {
        case "none":
                tui.Info("This provider is configured without authentication. Nothing to do.")
                return nil
        case "oauth":
                return runOAuthChoice(cfg, tui, providerID, pcfg)
        }

        choice := tui.Choice("Select authentication method [1-3, default 1]:", []string{
                "API key", "OAuth2 (if officially published)", "None (local server)"}, 1)
        switch choice {
        case 2:
                pcfg.Auth = "oauth"
                if err := cfg.Save(); err != nil {
                        return err
                }
                return runOAuthChoice(cfg, tui, providerID, pcfg)
        case 3:
                pcfg.Auth = "none"
                return cfg.Save()
        }

        key, err := tui.Password("Paste your API key (input hidden): ")
        if err != nil {
                return err
        }
        key = strings.TrimSpace(key)
        if key == "" {
                return fmt.Errorf("no API key entered; login cancelled")
        }
        if err := StoreSecret(providerID, "api_key", key); err != nil {
                return fmt.Errorf("could not store credential: %w", err)
        }
        pcfg.Auth = "api_key"
        if err := cfg.Save(); err != nil {
                return err
        }

        tui.Success("API key stored in the system credential store.")
        return nil
}

func runOAuthChoice(cfg *Config, tui *Terminal, providerID string, pcfg *ProviderConfig) error {
        if pcfg.OAuth == nil || pcfg.OAuth.TokenURL == "" {
                tui.Warn("No official third-party OAuth endpoints are configured for this provider.")
                tui.Block("Why", "ChatGPT (OpenAI) and Claude (Anthropic) subscription sign-in only works\n"+
                        "in their own first-party apps. Third-party tools must use API keys.\n"+
                        "You can still point rup.ai's OAuth client at any gateway that publishes\n"+
                        "OAuth (e.g. a self-hosted LiteLLM/Keycloak setup) via `rup.ai config`.")
                tui.Info("Falling back to API-key authentication.")
                pcfg.Auth = "api_key"
                if err := cfg.Save(); err != nil {
                        return err
                }
                return LoginFlow(cfg, tui, providerID)
        }
        switch pcfg.OAuth.Kind {
        case "device":
                return oauthDeviceFlow(cfg, tui, providerID, pcfg)
        default:
                return oauthCodeFlow(cfg, tui, providerID, pcfg)
        }
}

// oauthDeviceFlow implements RFC 8628 Device Authorization Grant.
func oauthDeviceFlow(cfg *Config, tui *Terminal, providerID string, pcfg *ProviderConfig) error {
        oc := pcfg.OAuth
        form := url.Values{"client_id": {oc.ClientID}, "scopes": {strings.Join(oc.Scopes, " ")}}
        resp, err := http.PostForm(oc.AuthURL, form)
        if err != nil {
                return err
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        var dev struct {
                DeviceCode      string `json:"device_code"`
                UserCode        string `json:"user_code"`
                VerificationURI string `json:"verification_uri"`
                VerificationURL string `json:"verification_url"`
                Interval        int    `json:"interval"`
                ExpiresIn       int    `json:"expires_in"`
                Error           string `json:"error"`
        }
        if err := json.Unmarshal(raw, &dev); err != nil || (dev.DeviceCode == "" && dev.Error == "") {
                return fmt.Errorf("device authorization endpoint returned an unexpected response: %s", truncateStr(string(raw), 200))
        }
        if dev.Error != "" {
                return fmt.Errorf("device authorization failed: %s", dev.Error)
        }
        uri := dev.VerificationURI
        if uri == "" {
                uri = dev.VerificationURL
        }
        tui.Block("Authorize rup.ai", fmt.Sprintf("1. Open:  %s\n2. Enter code:  %s", uri, dev.UserCode))
        interval := time.Duration(dev.Interval) * time.Second
        if interval <= 0 {
                interval = 5 * time.Second
        }
        deadline := time.Now().Add(10 * time.Minute)
        if dev.ExpiresIn > 0 && dev.ExpiresIn < 600 {
                deadline = time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
        }
        stop := tui.Spinner("Waiting for authorization")
        defer stop()
        for time.Now().Before(deadline) {
                time.Sleep(interval)
                token, err := oauthTokenRequest(oc.TokenURL, url.Values{
                        "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
                        "device_code": {dev.DeviceCode},
                        "client_id":  {oc.ClientID},
                })
                if err != nil {
                        continue
                }
                if token.AccessToken != "" {
                        stop()
                        return saveOAuthToken(cfg, tui, providerID, token)
                }
        }
        return fmt.Errorf("device authorization timed out")
}

// oauthCodeFlow implements OAuth2 Authorization Code + PKCE with a localhost
// callback, per RFC 6749 / RFC 7636.
func oauthCodeFlow(cfg *Config, tui *Terminal, providerID string, pcfg *ProviderConfig) error {
        oc := pcfg.OAuth
        verifier, err := randomB64(48)
        if err != nil {
                return err
        }
        sum := sha256.Sum256([]byte(verifier))
        challenge := base64.RawURLEncoding.EncodeToString(sum[:])

        state, err := randomB64(16)
        if err != nil {
                return err
        }
        listen, err := net.Listen("tcp", "127.0.0.1:0")
        if err != nil {
                return err
        }
        port := listen.Addr().(*net.TCPAddr).Port
        redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

        q := url.Values{}
        q.Set("response_type", "code")
        q.Set("client_id", oc.ClientID)
        q.Set("redirect_uri", redirect)
        q.Set("scope", strings.Join(oc.Scopes, " "))
        q.Set("state", state)
        q.Set("code_challenge", challenge)
        q.Set("code_challenge_method", "S256")
        authURL := oc.AuthURL + "?" + q.Encode()

        codeCh := make(chan string, 1)
        errCh := make(chan error, 1)
        mux := http.NewServeMux()
        srv := &http.Server{Handler: mux}
        mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "text/html; charset=utf-8")
                if r.URL.Query().Get("state") != state {
                        w.WriteHeader(http.StatusBadRequest)
                        fmt.Fprint(w, "<h1>rup.ai</h1><p>State mismatch — please restart login.</p>")
                        errCh <- fmt.Errorf("oauth state mismatch")
                        return
                }
                if e := r.URL.Query().Get("error"); e != "" {
                        w.WriteHeader(http.StatusOK)
                        fmt.Fprintf(w, "<h1>rup.ai</h1><p>Login failed: %s. You can close this tab.</p>", html.EscapeString(e))
                        errCh <- fmt.Errorf("provider returned %q", e)
                        return
                }
                code := r.URL.Query().Get("code")
                w.Header().Set("Content-Type", "text/html; charset=utf-8")
                fmt.Fprint(w, "<h1>rup.ai</h1><p>Authorized — you can close this tab and return to the terminal.</p>")
                codeCh <- code
        })
        go func() { _ = srv.Serve(listen) }()
        defer func() {
                ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
                defer cancel()
                _ = srv.Shutdown(ctx)
        }()

        tui.Block("Authorize rup.ai", "Opening your browser to complete sign-in...\n"+authURL)
        if err := openInBrowser(authURL); err != nil {
                tui.Info("Could not open a browser automatically; paste the URL above manually.")
        }

        select {
        case code := <-codeCh:
                token, err := oauthTokenRequest(oc.TokenURL, url.Values{
                        "grant_type": {"authorization_code"},
                        "code":       {code},
                        "redirect_uri": {redirect},
                        "client_id":  {oc.ClientID},
                        "code_verifier": {verifier},
                })
                if err != nil {
                        return err
                }
                return saveOAuthToken(cfg, tui, providerID, token)
        case err := <-errCh:
                return err
        case <-time.After(5 * time.Minute):
                return fmt.Errorf("authorization timed out after 5 minutes")
        }
}

type oauthToken struct {
        AccessToken  string `json:"access_token"`
        RefreshToken string `json:"refresh_token"`
        TokenType    string `json:"token_type"`
        ExpiresIn    int    `json:"expires_in"`
        Error        string `json:"error"`
        Description  string `json:"error_description"`
}

func oauthTokenRequest(tokenURL string, form url.Values) (*oauthToken, error) {
        resp, err := http.PostForm(tokenURL, form)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        var tok oauthToken
        if err := json.Unmarshal(raw, &tok); err != nil {
                return nil, fmt.Errorf("token endpoint returned invalid JSON: %s", truncateStr(string(raw), 200))
        }
        if tok.Error != "" {
                return nil, fmt.Errorf("%s: %s", tok.Error, tok.Description)
        }
        if tok.AccessToken == "" {
                return nil, fmt.Errorf("token endpoint returned no access token")
        }
        return &tok, nil
}

func saveOAuthToken(cfg *Config, tui *Terminal, providerID string, tok *oauthToken) error {
        if err := StoreSecret(providerID, "oauth_token", tok.AccessToken); err != nil {
                return err
        }
        if tok.RefreshToken != "" {
                _ = StoreSecret(providerID, "refresh_token", tok.RefreshToken)
        }
        if pcfg := cfg.Providers[providerID]; pcfg != nil {
                pcfg.Auth = "oauth"
        }
        if err := cfg.Save(); err != nil {
                return err
        }
        tui.Success("Signed in. OAuth token stored in the system credential store.")
        return nil
}

// Logout removes stored credentials for a provider.
func Logout(cfg *Config, tui *Terminal, providerID string) error {
        if providerID == "" {
                providerID = cfg.ActiveProvider
        }
        if providerID == "" {
                return fmt.Errorf("no active provider; use `rup.ai logout <provider>`")
        }
        if err := DeleteSecret(providerID, "api_key"); err != nil {
                tui.Warn(fmt.Sprintf("credential removal warning: %v", err))
        }
        _ = DeleteSecret(providerID, "oauth_token")
        _ = DeleteSecret(providerID, "refresh_token")
        tui.Success(fmt.Sprintf("Credentials for %q removed from this machine.", providerID))
        return nil
}

// randomB64 returns cryptographically random base64 (raw URL) bytes.
func randomB64(n int) (string, error) {
        b := make([]byte, n)
        if _, err := rand.Read(b); err != nil {
                return "", err
        }
        return base64.RawURLEncoding.EncodeToString(b), nil
}

// TestConnection verifies a provider by listing models, falling back to a
// 1-token chat for servers without /models. Returns nil on success.
func TestConnection(ctx context.Context, prov Provider) error {
        cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
        defer cancel()
        if _, err := prov.ListModels(cctx); err == nil {
                return nil
        }
        req := ChatRequest{
                Model:       "ping",
                Messages:    []Message{{Role: RoleUser, Content: "ping"}},
                MaxTokens:   1,
                Temperature: 0,
        }
        if _, err := prov.Stream(cctx, req, nil); err != nil {
                if isContextErr(err) {
                        return fmt.Errorf("connection test timed out")
                }
                return err
        }
        return nil
}
