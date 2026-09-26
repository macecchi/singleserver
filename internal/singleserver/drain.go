package singleserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	vectorVersion        = "0.58.0"
	vectorServiceName    = "vector-singleserver.service"
	defaultDrainEndpoint = "https://us.i.posthog.com/i/v1/logs"
)

type DrainState struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

func drainStatePath() string {
	return filepath.Join(envDefault("SINGLESERVER_STATE_DIR", "/etc/singleserver"), "drain.json")
}

func vectorConfigPath() string {
	return filepath.Join(envDefault("SINGLESERVER_STATE_DIR", "/etc/singleserver"), "vector.yaml")
}

func vectorDataDir() string {
	return envDefault("SINGLESERVER_VECTOR_DATA_DIR", "/var/lib/singleserver/vector")
}

func vectorBinPath() string {
	return envDefault("SINGLESERVER_VECTOR_BIN", "/usr/local/bin/vector")
}

func vectorUnitPath() string {
	return filepath.Join(envDefault("SINGLESERVER_SYSTEMD_DIR", "/etc/systemd/system"), vectorServiceName)
}

func loadDrainState() (*DrainState, error) {
	body, err := os.ReadFile(drainStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state DrainState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func writeDrainState(state *DrainState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(drainStatePath(), append(body, '\n'))
}

func cliDrain(args []string, w io.Writer) error {
	mode, args, err := commandModeFromArgs(args, drainFlagTakesValue)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return cliDrainStatus(w)
	}
	return withCLIMode(mode, func() error {
		switch args[0] {
		case "enable":
			return cliDrainEnable(args[1:], w)
		case "disable":
			return cliDrainDisable(args[1:], w)
		case "status":
			return cliDrainStatus(w)
		default:
			return fmt.Errorf("unknown drain command %q", args[0])
		}
	})
}

func drainFlagTakesValue(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if before, _, ok := strings.Cut(name, "="); ok {
		name = before
	}
	return name == "token" || name == "endpoint"
}

func cliDrainEnable(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("drain enable", flag.ContinueOnError)
	fs.SetOutput(w)
	token := fs.String("token", "", "bearer token for the OTLP endpoint")
	endpoint := fs.String("endpoint", "", "OTLP/HTTP logs endpoint")
	if err := fs.Parse(normalizeFlagArgs(args, drainFlagTakesValue)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: singleserver drain enable --token <token> [--endpoint <url>]")
	}
	state, err := loadDrainState()
	if err != nil {
		return err
	}
	if state == nil {
		state = &DrainState{}
	}
	if value := strings.TrimSpace(*token); value != "" {
		state.Token = value
	}
	if value := strings.TrimSpace(*endpoint); value != "" {
		state.Endpoint = value
	}
	if state.Endpoint == "" {
		state.Endpoint = defaultDrainEndpoint
	}
	if state.Token == "" {
		return errors.New("usage: singleserver drain enable --token <token> [--endpoint <url>]")
	}
	if !strings.HasPrefix(state.Endpoint, "https://") && !strings.HasPrefix(state.Endpoint, "http://") {
		return fmt.Errorf("--endpoint must be an http(s) URL, got %q", state.Endpoint)
	}

	if err := installVectorFunc(vectorBinPath()); err != nil {
		return err
	}
	writeCheck(w, "drain", "vector", "ok", vectorVersion, vectorBinPath())

	if err := os.MkdirAll(vectorDataDir(), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(vectorConfigPath(), []byte(renderVectorConfig(state, vectorDataDir()))); err != nil {
		return err
	}
	if err := commandRunFunc(30*time.Second, vectorBinPath(), "validate", "--no-environment", vectorConfigPath()); err != nil {
		return err
	}
	writeCheck(w, "drain", "config", "ok", vectorConfigPath())

	if err := writeDrainState(state); err != nil {
		return err
	}
	if err := writeVectorService(); err != nil {
		return err
	}
	if err := commandRunFunc(10*time.Second, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := commandRunFunc(10*time.Second, "systemctl", "enable", vectorServiceName); err != nil {
		return err
	}
	if err := commandRunFunc(30*time.Second, "systemctl", "restart", vectorServiceName); err != nil {
		return err
	}
	writeCheck(w, "drain", "service", "ok", vectorServiceName, "shipping app and deploy logs to "+state.Endpoint)
	return nil
}

func cliDrainDisable(args []string, w io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: singleserver drain disable")
	}
	if _, err := os.Stat(vectorUnitPath()); err == nil {
		if err := commandRunFunc(30*time.Second, "systemctl", "disable", "--now", vectorServiceName); err != nil {
			return err
		}
	}
	for _, path := range []string{vectorUnitPath(), vectorConfigPath(), drainStatePath()} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := commandRunFunc(10*time.Second, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	writeCheck(w, "drain", "service", "disabled", vectorServiceName)
	return nil
}

func cliDrainStatus(w io.Writer) error {
	state, err := loadDrainState()
	if err != nil {
		return err
	}
	if state == nil {
		writeCheck(w, "drain", "service", "disabled", "-", "run singleserver drain enable --token <token>")
		return nil
	}
	active, _ := commandOutputFunc(5*time.Second, "systemctl", "is-active", vectorServiceName)
	status := "ok"
	if strings.TrimSpace(active) != "active" {
		status = "failed"
	}
	writeCheck(w, "drain", "service", status, vectorServiceName, "state="+valueOrDash(strings.TrimSpace(active)))
	writeCheck(w, "drain", "endpoint", "ok", state.Endpoint)
	return nil
}

func writeVectorService() error {
	body := fmt.Sprintf(`[Unit]
Description=Single Server log drain
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, vectorBinPath(), vectorConfigPath())
	if err := os.MkdirAll(filepath.Dir(vectorUnitPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(vectorUnitPath(), []byte(body), 0644)
}

// Vector interpolates ${VAR} in config values, so a literal $ is written as $$.
func vectorYAMLString(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "$", "$$"))
}

func renderVectorConfig(state *DrainState, dataDir string) string {
	return fmt.Sprintf(`# Generated by singleserver drain enable. Changes are overwritten.
data_dir: %s
sources:
  journal:
    type: journald
    include_units: [docker.service, singleserver.service]
    since_now: true
transforms:
  otlp:
    type: remap
    inputs: [journal]
    source: |
%s
sinks:
  drain:
    type: opentelemetry
    inputs: [otlp]
    protocol:
      type: http
      uri: %s
      method: post
      auth:
        strategy: bearer
        token: %s
      encoding:
        codec: otlp
      batch:
        timeout_secs: 5
`, vectorYAMLString(dataDir), indentLines(vectorRemapSource, "      "), vectorYAMLString(state.Endpoint), vectorYAMLString(state.Token))
}

func indentLines(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// Maps a journald entry to one OTLP log record: app containers are named by their
// journald tag, the deploy daemon is "singleserver", and JSON lines are unpacked.
const vectorRemapSource = `unit = string(._SYSTEMD_UNIT) ?? ""
tag = string(.CONTAINER_TAG) ?? ""
if unit == "docker.service" && tag == "" { abort }
service = if tag != "" { tag } else { "singleserver" }
raw = string(.message) ?? ""
priority = to_int(.PRIORITY) ?? 6
severity = if priority <= 3 { "ERROR" } else if priority == 4 { "WARN" } else if priority >= 7 { "DEBUG" } else { "INFO" }
body = raw
attributes = []
parsed, err = parse_json(raw)
if err == null && is_object(parsed) {
  fields = object!(parsed)
  level = fields.severity || fields.level
  if is_string(level) { severity = upcase!(level) }
  if exists(fields.message) {
    body = if is_string(fields.message) { string!(fields.message) } else { encode_json(fields.message) }
  }
  fields = remove!(fields, ["message"])
  fields = remove!(fields, ["severity"])
  fields = remove!(fields, ["level"])
  fields = remove!(fields, ["timestamp"])
  for_each(fields) -> |key, value| {
    text = if is_string(value) { string!(value) } else { encode_json(value) }
    attributes = push(attributes, {"key": key, "value": {"stringValue": text}})
  }
}
if severity == "WARNING" { severity = "WARN" }
if severity == "CRITICAL" || severity == "PANIC" { severity = "FATAL" }
numbers = {"TRACE": 1, "DEBUG": 5, "INFO": 9, "NOTICE": 10, "WARN": 13, "ERROR": 17, "FATAL": 21}
number = get(numbers, [severity]) ?? 9
container = string(.CONTAINER_NAME) ?? ""
if container != "" {
  attributes = push(attributes, {"key": "container.name", "value": {"stringValue": container}})
}
. = {"resourceLogs": [{
  "resource": {"attributes": [
    {"key": "service.name", "value": {"stringValue": service}},
    {"key": "host.name", "value": {"stringValue": get_hostname!()}}
  ]},
  "scopeLogs": [{
    "scope": {"name": "singleserver"},
    "logRecords": [{
      "timeUnixNano": to_unix_timestamp(timestamp(.timestamp) ?? now(), unit: "nanoseconds"),
      "observedTimeUnixNano": to_unix_timestamp(now(), unit: "nanoseconds"),
      "severityText": severity,
      "severityNumber": number,
      "body": {"stringValue": body},
      "attributes": attributes
    }]
  }]
}]}
`

var installVectorFunc = installVector

func installVector(binPath string) error {
	if out, err := commandOutputFunc(10*time.Second, binPath, "--version"); err == nil && strings.Contains(out, "vector "+vectorVersion+" ") {
		return nil
	}
	target, err := vectorTarget(runtime.GOARCH)
	if err != nil {
		return err
	}
	archive := fmt.Sprintf("vector-%s-%s.tar.gz", vectorVersion, target)
	base := fmt.Sprintf("https://github.com/vectordotdev/vector/releases/download/v%s/", vectorVersion)
	sums, err := httpGetBytes(base + fmt.Sprintf("vector-%s-SHA256SUMS", vectorVersion))
	if err != nil {
		return err
	}
	expected, err := checksumFor(string(sums), archive)
	if err != nil {
		return err
	}
	body, err := httpGetBytes(base + archive)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != expected {
		return fmt.Errorf("checksum verification failed for %s", archive)
	}
	binary, err := extractVectorBinary(body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0755); err != nil {
		return err
	}
	tmp := binPath + ".tmp"
	if err := os.WriteFile(tmp, binary, 0755); err != nil {
		return err
	}
	return os.Rename(tmp, binPath)
}

func vectorTarget(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x86_64-unknown-linux-musl", nil
	case "arm64":
		return "aarch64-unknown-linux-musl", nil
	default:
		return "", fmt.Errorf("log drain does not support architecture %s", goarch)
	}
}

func checksumFor(sums, file string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s", file)
}

func extractVectorBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil, errors.New("vector binary not found in release archive")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg && strings.HasSuffix(header.Name, "/bin/vector") {
			return io.ReadAll(reader)
		}
	}
}

func httpGetBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
