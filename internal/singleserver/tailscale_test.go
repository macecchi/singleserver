package singleserver

import (
	"bytes"
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

func TestReportTailscaleAuthKeyAge(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	stored := func(daysAgo int) string {
		return now.AddDate(0, 0, -daysAgo).Format(time.RFC3339)
	}

	var fresh strings.Builder
	reportTailscaleAuthKeyAge(&fresh, &TailscaleState{AuthKey: "k", AuthKeyStoredAt: stored(10)}, now)
	if !strings.Contains(fresh.String(), "ok") || !strings.Contains(fresh.String(), "stored 10d ago") {
		t.Fatalf("expected ok for a fresh key, got: %q", fresh.String())
	}

	var aging strings.Builder
	reportTailscaleAuthKeyAge(&aging, &TailscaleState{AuthKey: "k", AuthKeyStoredAt: stored(80)}, now)
	if !strings.Contains(aging.String(), "pending") || !strings.Contains(aging.String(), "expires by day 90") {
		t.Fatalf("expected pending for an aging key, got: %q", aging.String())
	}

	var expired strings.Builder
	reportTailscaleAuthKeyAge(&expired, &TailscaleState{AuthKey: "k", AuthKeyStoredAt: stored(120)}, now)
	if !strings.Contains(expired.String(), "past the 90-day cap") {
		t.Fatalf("expected expired warning, got: %q", expired.String())
	}

	var silent strings.Builder
	reportTailscaleAuthKeyAge(&silent, &TailscaleState{}, now)
	reportTailscaleAuthKeyAge(&silent, &TailscaleState{AuthKey: "k"}, now)
	if silent.String() != "" {
		t.Fatalf("expected no output without a dated key, got: %q", silent.String())
	}
}
