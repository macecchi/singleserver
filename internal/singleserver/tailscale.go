package singleserver

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type TailscaleState struct {
	Hostname  string `json:"hostname"`
	FunnelURL string `json:"funnel_url"`
	AuthKey   string `json:"auth_key,omitempty"`
}

type tailscaleSelf struct {
	DNSName      string   `json:"DNSName"`
	HostName     string   `json:"HostName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	KeyExpiry    string   `json:"KeyExpiry"`
	Tags         []string `json:"Tags"`
}

type tailscaleStatus struct {
	BackendState string         `json:"BackendState"`
	Self         *tailscaleSelf `json:"Self"`
}

var tailscaleFunnelReadyFunc = waitForTailscaleFunnelReady

func cliTailscaleConnect(args []string, w io.Writer) error {
	_, args, err := commandModeFromArgs(args, tailscaleFlagTakesValue)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("connect tailscale", flag.ContinueOnError)
	fs.SetOutput(w)
	authKey := fs.String("auth-key", defaultTailscaleAuthKey(), "Tailscale auth key")
	hostname := fs.String("hostname", strings.TrimSpace(os.Getenv("SINGLESERVER_TAILSCALE_HOSTNAME")), "Tailscale hostname")
	if err := fs.Parse(normalizeFlagArgs(args, tailscaleFlagTakesValue)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: singleserver connect tailscale [--auth-key <key>] [--hostname <name>]")
	}
	if err := ensureBaseFiles(); err != nil {
		return err
	}
	if _, err := commandOutputFunc(5*time.Second, "tailscale", "version"); err != nil {
		return fmt.Errorf("tailscale is not installed; rerun the Single Server installer: %w", err)
	}
	if err := commandRunFunc(20*time.Second, "systemctl", "enable", "--now", "tailscaled"); err != nil {
		return err
	}

	status, err := currentTailscaleStatus()
	if err != nil || !tailscaleRunning(status) {
		if strings.TrimSpace(*authKey) == "" {
			writeCheck(w, "tailscale", "login", "pending", "run `tailscale up --ssh` on this server, then run `singleserver connect tailscale`")
			return nil
		}
		upArgs := []string{"up", "--ssh", "--auth-key=" + strings.TrimSpace(*authKey)}
		if strings.TrimSpace(*hostname) != "" {
			upArgs = append(upArgs, "--hostname="+strings.TrimSpace(*hostname))
		}
		if err := commandRunFunc(2*time.Minute, "tailscale", upArgs...); err != nil {
			return err
		}
		status, err = currentTailscaleStatus()
		if err != nil {
			return err
		}
	}
	if !tailscaleRunning(status) {
		writeCheck(w, "tailscale", "login", "pending", "run `tailscale up --ssh` on this server, then run `singleserver connect tailscale`")
		return nil
	}
	writeCheck(w, "tailscale", "status", "ok", tailscaleStatusName(status))
	reportTailscaleKeyExpiry(w, status)

	if err := commandRunFunc(15*time.Second, "tailscale", "set", "--ssh"); err != nil {
		writeCheck(w, "tailscale", "ssh", "pending", err.Error())
	} else {
		writeCheck(w, "tailscale", "ssh", "ok", "-")
	}

	port := envDefault("SINGLESERVER_PORT", "8787")
	writeCheck(w, "tailscale", "funnel", "starting", "127.0.0.1:"+port)
	if err := commandRunToWriterFunc(w, 45*time.Second, "tailscale", "funnel", "--bg", "--yes", port); err != nil {
		writeCheck(w, "tailscale", "funnel", "pending", err.Error())
		return writeTailscaleStateFromStatus(status, "", *authKey)
	}
	status, err = currentTailscaleStatus()
	if err != nil {
		return err
	}
	funnelURL := tailscaleFunnelURL(status)
	if funnelURL == "" {
		writeCheck(w, "tailscale", "funnel", "pending", "-", "could not determine Funnel URL from tailscale status")
		return writeTailscaleStateFromStatus(status, "", *authKey)
	}
	if err := writeTailscaleStateFromStatus(status, funnelURL, *authKey); err != nil {
		return err
	}
	env, err := loadServiceEnv()
	if err != nil {
		return err
	}
	env["SINGLESERVER_PUBLIC_URL"] = funnelURL
	if err := writeServiceEnv(env); err != nil {
		return err
	}
	if err := commandRunFunc(10*time.Second, "systemctl", "restart", "singleserver.service"); err != nil {
		return err
	}
	if err := tailscaleFunnelReadyFunc(funnelURL, 90*time.Second); err != nil {
		writeCheck(w, "tailscale", "funnel", "pending", funnelURL, err.Error())
		return nil
	}
	writeCheck(w, "tailscale", "funnel", "ok", funnelURL, "target=127.0.0.1:"+port)
	return nil
}

func waitForTailscaleFunnelReady(funnelURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	healthURL := strings.TrimRight(funnelURL, "/") + "/health"
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			cancel()
			return err
		}
		res, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				cancel()
				return nil
			}
			lastErr = fmt.Errorf("GET %s returned %s", healthURL, res.Status)
		} else {
			lastErr = err
		}
		cancel()
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("Funnel healthcheck did not become ready: %w", lastErr)
	}
	return errors.New("Funnel healthcheck did not become ready")
}

func currentTailscaleStatus() (*tailscaleStatus, error) {
	body, err := commandOutputFunc(5*time.Second, "tailscale", "status", "--json")
	if err != nil {
		return nil, err
	}
	var status tailscaleStatus
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func tailscaleRunning(status *tailscaleStatus) bool {
	return status != nil && strings.EqualFold(status.BackendState, "Running") && status.Self != nil
}

func tailscaleStatusName(status *tailscaleStatus) string {
	if status == nil || status.Self == nil {
		return "-"
	}
	if name := strings.TrimSuffix(strings.TrimSpace(status.Self.DNSName), "."); name != "" {
		return name
	}
	if name := strings.TrimSpace(status.Self.HostName); name != "" {
		return name
	}
	return "-"
}

func tailscaleFunnelURL(status *tailscaleStatus) string {
	host := tailscaleStatusName(status)
	if host == "-" || !strings.Contains(host, ".ts.net") {
		return ""
	}
	return "https://" + host
}

func writeTailscaleStateFromStatus(status *tailscaleStatus, funnelURL string, authKey string) error {
	existing, _ := loadTailscaleState()
	if authKey == "" && existing != nil {
		authKey = existing.AuthKey
	}
	state := &TailscaleState{
		Hostname:  tailscaleStatusName(status),
		FunnelURL: strings.TrimRight(funnelURL, "/"),
		AuthKey:   authKey,
	}
	return writeTailscaleState(state)
}

func loadTailscaleState() (*TailscaleState, error) {
	body, err := os.ReadFile(tailscaleStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &TailscaleState{}, nil
		}
		return nil, err
	}
	var state TailscaleState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func writeTailscaleState(state *TailscaleState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(tailscaleStatePath(), append(body, '\n'))
}

func tailscaleStatePath() string {
	return filepath.Join(envDefault("SINGLESERVER_STATE_DIR", "/etc/singleserver"), "tailscale.json")
}

func tailscaleFlagTakesValue(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if before, _, ok := strings.Cut(name, "="); ok {
		name = before
	}
	return name == "auth-key" || name == "hostname"
}

func defaultTailscaleAuthKey() string {
	return strings.TrimSpace(os.Getenv("TAILSCALE_AUTHKEY"))
}

func doctorTailscale(w io.Writer, appCount int) bool {
	if _, err := commandOutputFunc(5*time.Second, "tailscale", "version"); err != nil {
		status := "pending"
		if appCount > 0 {
			status = "failed"
		}
		writeCheck(w, "tailscale", "setup", status, "install Tailscale", err.Error())
		return appCount == 0
	}
	status, err := currentTailscaleStatus()
	if err != nil || !tailscaleRunning(status) {
		state := "pending"
		if appCount > 0 {
			state = "failed"
		}
		if err != nil {
			writeCheck(w, "tailscale", "setup", state, "run `tailscale up --ssh`", err.Error())
		} else {
			writeCheck(w, "tailscale", "setup", state, "run `tailscale up --ssh`")
		}
		return appCount == 0
	}
	writeCheck(w, "tailscale", "status", "ok", tailscaleStatusName(status))
	reportTailscaleKeyExpiry(w, status)

	env, _ := loadServiceEnv()
	publicURL := strings.TrimRight(env["SINGLESERVER_PUBLIC_URL"], "/")
	if publicURL == "" {
		state, _ := loadTailscaleState()
		publicURL = strings.TrimRight(state.FunnelURL, "/")
	}
	if publicURL == "" {
		status := "pending"
		if appCount > 0 {
			status = "failed"
		}
		writeCheck(w, "tailscale", "funnel", status, "run `singleserver connect tailscale`")
		return appCount == 0
	}
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Scheme != "https" || !strings.HasSuffix(parsed.Hostname(), ".ts.net") {
		writeCheck(w, "tailscale", "funnel", "failed", publicURL, "expected Tailscale Funnel URL")
		return false
	}
	if err := commandRunFunc(5*time.Second, "tailscale", "funnel", "status", "--json"); err != nil {
		status := "pending"
		if appCount > 0 {
			status = "failed"
		}
		writeCheck(w, "tailscale", "funnel", status, err.Error())
		return appCount == 0
	}
	writeCheck(w, "tailscale", "funnel", "ok", publicURL)
	return true
}

// tailscaleAdminURL returns the admin-console page for this machine, so we can
// link the user straight to the one-click "Disable key expiry" toggle.
func tailscaleAdminURL(status *tailscaleStatus) string {
	if status == nil || status.Self == nil {
		return ""
	}
	for _, ip := range status.Self.TailscaleIPs {
		if strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
			return "https://login.tailscale.com/admin/machines/" + ip
		}
	}
	return ""
}

// reportTailscaleKeyExpiry warns when the node's key is set to expire. An expired
// key drops the box off the tailnet, which kills Funnel webhooks and Tailscale
// SSH, so deploys silently stop. Tagged nodes and nodes with key expiry disabled
// never expire. The fix is one click in the admin console, so we link straight to
// the machine. Used by both `connect tailscale` and `doctor`.
func reportTailscaleKeyExpiry(w io.Writer, status *tailscaleStatus) {
	if status == nil || status.Self == nil {
		return
	}
	if len(status.Self.Tags) > 0 {
		writeCheck(w, "tailscale", "key expiry", "ok", "disabled (tagged)")
		return
	}
	expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(status.Self.KeyExpiry))
	if err != nil || expiry.IsZero() {
		writeCheck(w, "tailscale", "key expiry", "ok", "disabled")
		return
	}
	days := int(time.Until(expiry).Hours() / 24)
	detail := "disable key expiry so this box stays online"
	if url := tailscaleAdminURL(status); url != "" {
		detail += ": " + url
	}
	writeCheck(w, "tailscale", "key expiry", "pending",
		fmt.Sprintf("expires %s (%dd)", expiry.Format("2006-01-02"), days), detail)
}

func syncTailscaleAppDomain(app AppConfig, hostname string, add bool, w io.Writer) error {
	if !strings.Contains(hostname, ".") {
		if domain := tailnetDomain(); domain != "" {
			hostname = hostname + "." + domain
		}
	}
	state, err := loadTailscaleState()
	if err != nil {
		return err
	}
	authKey := state.AuthKey
	if authKey == "" {
		authKey = defaultTailscaleAuthKey()
	}
	if authKey == "" && add {
		return errors.New("Tailscale auth key is required to configure private tunnels; run `singleserver connect tailscale --auth-key <key>` first or set TAILSCALE_AUTHKEY in singleserver.env")
	}

	// The app is served at the node's own MagicDNS name, so the node hostname
	// must be the first label of the configured domain or the two never match.
	appNodeName := "singleserver-" + app.Name
	if label, _, ok := strings.Cut(hostname, "."); ok && label != "" {
		appNodeName = label
	}
	socketPath := fmt.Sprintf("/run/tailscaled-%s.sock", app.Name)
	stateDir := fmt.Sprintf("/var/lib/tailscale/tailscaled-%s", app.Name)

	if add {
		// 1. Ensure systemd template service exists
		if err := ensureTailscaleTemplateService(); err != nil {
			return err
		}

		// 2. Ensure statedir exists
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			return err
		}

		// 3. Start the service for this app
		serviceName := fmt.Sprintf("tailscaled-app@%s.service", app.Name)
		writeCheck(w, app.Name, "tailscale_service", "starting", serviceName)
		if err := commandRunFunc(15*time.Second, "systemctl", "enable", "--now", serviceName); err != nil {
			return err
		}

		// Wait a second for tailscaled socket to appear
		time.Sleep(1 * time.Second)

		// 4. Authenticate the node
		writeCheck(w, app.Name, "tailscale_up", "authenticating", appNodeName)
		upArgs := []string{
			"--socket=" + socketPath,
			"up",
			"--ssh",
			"--auth-key=" + authKey,
			"--hostname=" + appNodeName,
		}
		if err := commandRunFunc(2*time.Minute, "tailscale", upArgs...); err != nil {
			return fmt.Errorf("failed to tailscale up for %s: %w", app.Name, err)
		}

		// 5. Run serve
		writeCheck(w, app.Name, "tailscale_serve", "configuring", "target=127.0.0.1:80")
		serveArgs := []string{
			"--socket=" + socketPath,
			"serve",
			"--bg",
			"--yes",
			"http://127.0.0.1:80",
		}
		if err := commandRunFunc(30*time.Second, "tailscale", serveArgs...); err != nil {
			return fmt.Errorf("failed to tailscale serve for %s: %w", app.Name, err)
		}

		writeCheck(w, app.Name, "tailscale_tunnel", "ok", hostname)
	} else {
		// Stop and disable service
		serviceName := fmt.Sprintf("tailscaled-app@%s.service", app.Name)
		writeCheck(w, app.Name, "tailscale_service", "stopping", serviceName)
		_ = commandRunFunc(15*time.Second, "systemctl", "disable", "--now", serviceName)

		// Remove statedir
		_ = os.RemoveAll(stateDir)
		_ = os.Remove(socketPath)

		writeCheck(w, app.Name, "tailscale_tunnel", "ok", "removed")
	}

	return nil
}

func tailnetDomain() string {
	state, err := loadTailscaleState()
	if err != nil || state.Hostname == "" {
		return ""
	}
	return tailnetDomainFromHostname(state.Hostname)
}

func tailnetDomainFromHostname(hostname string) string {
	parts := strings.Split(hostname, ".")
	if len(parts) >= 3 && parts[len(parts)-2] == "ts" && parts[len(parts)-1] == "net" {
		return strings.Join(parts[1:], ".")
	}
	return ""
}

func ensureTailscaleTemplateService() error {
	path := "/etc/systemd/system/tailscaled-app@.service"
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tailscaledPath, err := exec.LookPath("tailscaled")
	if err != nil {
		tailscaledPath = "/usr/sbin/tailscaled"
	}
	body := fmt.Sprintf(`[Unit]
Description=Tailscale node for Single Server App %%i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --socket=/run/tailscaled-%%i.sock --statedir=/var/lib/tailscale/tailscaled-%%i --port=0 --tun=userspace-networking
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, tailscaledPath)
	if err := os.MkdirAll("/etc/systemd/system", 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0644)
}
