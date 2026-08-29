package singleserver

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDoctorTailscaleKeyExpiry(t *testing.T) {
	soon := time.Now().Add(60 * 24 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name string
		self *tailscaleSelf
		want string
	}{
		{"tagged node never expires", &tailscaleSelf{Tags: []string{"tag:server"}, KeyExpiry: soon}, "key expiry\tok"},
		{"expiry disabled (zero time)", &tailscaleSelf{KeyExpiry: "0001-01-01T00:00:00Z"}, "key expiry\tok"},
		{"no expiry field", &tailscaleSelf{}, "key expiry\tok"},
		{"untagged with future expiry warns", &tailscaleSelf{KeyExpiry: soon}, "key expiry\tpending"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportTailscaleKeyExpiry(&buf, &tailscaleStatus{Self: c.self})
			if !strings.Contains(buf.String(), c.want) {
				t.Fatalf("expected %q, got: %q", c.want, buf.String())
			}
		})
	}

	// An expiring node with a Tailscale IP gets a deep link to its admin page.
	var linkBuf bytes.Buffer
	reportTailscaleKeyExpiry(&linkBuf, &tailscaleStatus{Self: &tailscaleSelf{KeyExpiry: soon, TailscaleIPs: []string{"100.64.0.1"}}})
	if !strings.Contains(linkBuf.String(), "login.tailscale.com/admin/machines/100.64.0.1") {
		t.Fatalf("expected admin deep link, got: %q", linkBuf.String())
	}
}

func TestReportTailscaleAuthKeyStatus(t *testing.T) {
	var silent strings.Builder
	reportTailscaleAuthKeyStatus(&silent, &TailscaleState{AuthKey: "k"})
	reportTailscaleAuthKeyStatus(&silent, &TailscaleState{AuthKeyFailedAt: "2026-08-01T00:00:00Z"})
	if silent.String() != "" {
		t.Fatalf("expected silence without a known failure, got: %q", silent.String())
	}

	var failed strings.Builder
	reportTailscaleAuthKeyStatus(&failed, &TailscaleState{AuthKey: "k", AuthKeyFailedAt: "2026-08-01T00:00:00Z"})
	out := failed.String()
	if !strings.Contains(out, "failed") || !strings.Contains(out, "rejected 2026-08-01") {
		t.Fatalf("expected failure report, got: %q", out)
	}
}

func TestTailscaleAuthErr(t *testing.T) {
	if !tailscaleAuthErr(errors.New("backend error: invalid key: unable to validate API key")) {
		t.Fatal("expected invalid key to be an auth error")
	}
	if tailscaleAuthErr(errors.New("dial tcp: connection refused")) {
		t.Fatal("expected network error not to be an auth error")
	}
	if tailscaleAuthErr(nil) {
		t.Fatal("expected nil not to be an auth error")
	}
}

func TestPromptTailscaleAuthKey(t *testing.T) {
	origInteractive := addPromptInteractiveFunc
	origInput := addPromptInput
	t.Cleanup(func() {
		addPromptInteractiveFunc = origInteractive
		addPromptInput = origInput
	})

	addPromptInteractiveFunc = func() bool { return false }
	if key, err := promptTailscaleAuthKey(&bytes.Buffer{}); err != nil || key != "" {
		t.Fatalf("expected silent skip when not interactive, got %q, %v", key, err)
	}

	addPromptInteractiveFunc = func() bool { return true }
	addPromptInput = strings.NewReader("not-a-key\ntskey-auth-abc123\n")
	var out bytes.Buffer
	key, err := promptTailscaleAuthKey(&out)
	if err != nil {
		t.Fatal(err)
	}
	if key != "tskey-auth-abc123" {
		t.Fatalf("expected the valid key after a retry, got %q", key)
	}
	for _, want := range []string{"Reusable", "tag:singleserver", "doesn't look right"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected output to contain %q:\n%s", want, out.String())
		}
	}

	addPromptInput = strings.NewReader("\n")
	if key, err := promptTailscaleAuthKey(&bytes.Buffer{}); err != nil || key != "" {
		t.Fatalf("expected empty on skip, got %q, %v", key, err)
	}
}
