package singleserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupDrainTest(t *testing.T) (string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SINGLESERVER_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("SINGLESERVER_VECTOR_DATA_DIR", filepath.Join(dir, "vector-data"))
	t.Setenv("SINGLESERVER_VECTOR_BIN", filepath.Join(dir, "bin", "vector"))
	t.Setenv("SINGLESERVER_SYSTEMD_DIR", filepath.Join(dir, "systemd"))
	calls := []string{}
	originalRun := commandRunFunc
	originalInstall := installVectorFunc
	originalOutput := commandOutputFunc
	t.Cleanup(func() {
		commandRunFunc = originalRun
		installVectorFunc = originalInstall
		commandOutputFunc = originalOutput
	})
	commandRunFunc = func(timeout time.Duration, name string, args ...string) error {
		calls = append(calls, filepath.Base(name)+" "+strings.Join(args, " "))
		return nil
	}
	installVectorFunc = func(binPath string) error {
		calls = append(calls, "install "+binPath)
		return nil
	}
	commandOutputFunc = func(timeout time.Duration, name string, args ...string) (string, error) {
		return "active", nil
	}
	return dir, &calls
}

func TestDrainEnableWritesConfigUnitAndStartsService(t *testing.T) {
	dir, calls := setupDrainTest(t)
	var out bytes.Buffer
	if err := cliDrain([]string{"enable", "--token", "phc_test"}, &out); err != nil {
		t.Fatal(err)
	}

	config, err := os.ReadFile(filepath.Join(dir, "state", "vector.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`uri: "https://us.i.posthog.com/i/v1/logs"`,
		`token: "phc_test"`,
		"codec: otlp",
		"include_units: [docker.service, singleserver.service]",
		"since_now: true",
		`      service = if tag != "" { tag } else { "singleserver" }`,
	} {
		if !strings.Contains(string(config), want) {
			t.Fatalf("vector config missing %q:\n%s", want, config)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "state", "vector.yaml")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("vector config must be 0600 since it holds the token, got %v %v", info, err)
	}

	unit, err := os.ReadFile(filepath.Join(dir, "systemd", vectorServiceName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStart="+filepath.Join(dir, "bin", "vector")+" --config "+filepath.Join(dir, "state", "vector.yaml")) {
		t.Fatalf("unexpected unit:\n%s", unit)
	}

	wantCalls := []string{
		"install " + filepath.Join(dir, "bin", "vector"),
		"vector validate --no-environment " + filepath.Join(dir, "state", "vector.yaml"),
		"systemctl daemon-reload",
		"systemctl enable " + vectorServiceName,
		"systemctl restart " + vectorServiceName,
	}
	if strings.Join(*calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("calls = %q, want %q", *calls, wantCalls)
	}

	state, err := loadDrainState()
	if err != nil || state == nil || state.Token != "phc_test" || state.Endpoint != defaultDrainEndpoint {
		t.Fatalf("unexpected state %+v %v", state, err)
	}
}

func TestDrainEnableKeepsStoredTokenWhenOnlyEndpointChanges(t *testing.T) {
	_, _ = setupDrainTest(t)
	var out bytes.Buffer
	if err := cliDrain([]string{"enable", "--token", "phc_test"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cliDrain([]string{"enable", "--endpoint", "https://eu.i.posthog.com/i/v1/logs"}, &out); err != nil {
		t.Fatal(err)
	}
	state, _ := loadDrainState()
	if state.Token != "phc_test" || state.Endpoint != "https://eu.i.posthog.com/i/v1/logs" {
		t.Fatalf("unexpected state %+v", state)
	}
}

func TestDrainEnableRequiresToken(t *testing.T) {
	_, calls := setupDrainTest(t)
	var out bytes.Buffer
	err := cliDrain([]string{"enable"}, &out)
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("expected token usage error, got %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no side effects, got %q", *calls)
	}
}

func TestDrainDisableRemovesTokenBearingFiles(t *testing.T) {
	dir, calls := setupDrainTest(t)
	var out bytes.Buffer
	if err := cliDrain([]string{"enable", "--token", "phc_test"}, &out); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	if err := cliDrain([]string{"disable"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "state", "vector.yaml"),
		filepath.Join(dir, "state", "drain.json"),
		filepath.Join(dir, "systemd", vectorServiceName),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, got %v", path, err)
		}
	}
	if (*calls)[0] != "systemctl disable --now "+vectorServiceName {
		t.Fatalf("unexpected calls %q", *calls)
	}
}

func TestDrainStatusReportsDisabledWithoutState(t *testing.T) {
	_, _ = setupDrainTest(t)
	var out bytes.Buffer
	if err := cliDrain(nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "drain\tservice\tdisabled") {
		t.Fatalf("unexpected status output %q", out.String())
	}
}

func TestVectorYAMLStringEscapesInterpolation(t *testing.T) {
	if got := vectorYAMLString("a$b"); got != `"a$$b"` {
		t.Fatalf("got %s", got)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc  vector-0.58.0-x86_64-unknown-linux-musl.tar.gz\ndef  vector-0.58.0-aarch64-unknown-linux-musl.tar.gz\n"
	got, err := checksumFor(sums, "vector-0.58.0-aarch64-unknown-linux-musl.tar.gz")
	if err != nil || got != "def" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := checksumFor(sums, "missing.tar.gz"); err == nil {
		t.Fatal("expected error for missing checksum")
	}
}

func TestExtractVectorBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"./vector-aarch64-unknown-linux-musl/README.md":  "readme",
		"./vector-aarch64-unknown-linux-musl/bin/vector": "binary",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	got, err := extractVectorBinary(buf.Bytes())
	if err != nil || string(got) != "binary" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestVectorTarget(t *testing.T) {
	if got, _ := vectorTarget("arm64"); got != "aarch64-unknown-linux-musl" {
		t.Fatalf("arm64 -> %s", got)
	}
	if got, _ := vectorTarget("amd64"); got != "x86_64-unknown-linux-musl" {
		t.Fatalf("amd64 -> %s", got)
	}
	if _, err := vectorTarget("riscv64"); err == nil {
		t.Fatal("expected unsupported arch error")
	}
}
