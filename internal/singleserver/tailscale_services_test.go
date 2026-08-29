package singleserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withFakeTailscaleAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	original := tailscaleAPIBaseURL
	tailscaleAPIBaseURL = server.URL
	t.Cleanup(func() {
		tailscaleAPIBaseURL = original
		server.Close()
	})
}

func TestTailscaleAPIToken(t *testing.T) {
	withFakeTailscaleAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/oauth/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != "id" || r.PostForm.Get("client_secret") != "secret" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})

	token, err := tailscaleAPIToken("id", "secret")
	if err != nil || token != "tok" {
		t.Fatalf("expected token, got %q, %v", token, err)
	}
	if _, err := tailscaleAPIToken("id", "wrong"); err == nil {
		t.Fatal("expected error for bad credentials")
	}
}

func TestEnsureVIPServicePreservesAddrs(t *testing.T) {
	var putBody vipService
	withFakeTailscaleAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(vipService{Name: "svc:app", Addrs: []string{"100.99.0.1"}})
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})

	if err := ensureVIPService("tok", "app", "Single Server app app"); err != nil {
		t.Fatal(err)
	}
	if putBody.Name != "svc:app" || len(putBody.Addrs) != 1 || putBody.Addrs[0] != "100.99.0.1" {
		t.Fatalf("expected existing addrs preserved, got %+v", putBody)
	}
	if len(putBody.Tags) != 1 || putBody.Tags[0] != tailscaleServiceTag {
		t.Fatalf("expected service tagged %s, got %+v", tailscaleServiceTag, putBody.Tags)
	}
	if len(putBody.Ports) != 1 || putBody.Ports[0] != "tcp:443" {
		t.Fatalf("expected tcp:443 port, got %+v", putBody.Ports)
	}
}

func TestDeleteVIPServiceTolerates404(t *testing.T) {
	withFakeTailscaleAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	if err := deleteVIPService("tok", "gone"); err != nil {
		t.Fatal(err)
	}
}

func TestPromptTailscaleOAuthClient(t *testing.T) {
	origInteractive := addPromptInteractiveFunc
	origInput := addPromptInput
	t.Cleanup(func() {
		addPromptInteractiveFunc = origInteractive
		addPromptInput = origInput
	})

	addPromptInteractiveFunc = func() bool { return false }
	if id, secret, err := promptTailscaleOAuthClient(&bytes.Buffer{}); err != nil || id != "" || secret != "" {
		t.Fatalf("expected silent skip when not interactive, got %q/%q, %v", id, secret, err)
	}

	withFakeTailscaleAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_secret") != "good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})

	addPromptInteractiveFunc = func() bool { return true }
	addPromptInput = strings.NewReader("my-id\nbad\nmy-id\ngood\n")
	var out bytes.Buffer
	id, secret, err := promptTailscaleOAuthClient(&out)
	if err != nil {
		t.Fatal(err)
	}
	if id != "my-id" || secret != "good" {
		t.Fatalf("expected validated credentials after retry, got %q/%q", id, secret)
	}
	for _, want := range []string{"autoApprovers", "Trust credentials", "didn't work"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("expected output to contain %q:\n%s", want, out.String())
		}
	}

	addPromptInput = strings.NewReader("\n")
	if id, _, err := promptTailscaleOAuthClient(&bytes.Buffer{}); err != nil || id != "" {
		t.Fatalf("expected empty on skip, got %q, %v", id, err)
	}
}

func TestTailscaleServiceNameDerivation(t *testing.T) {
	cases := []struct {
		name string
		app  AppConfig
		want string
	}{
		{"first label of the domain", AppConfig{Name: "scoreboard", Hosts: []string{"scores.corp.ts.net"}}, "scores"},
		{"bare label domain", AppConfig{Name: "scoreboard", Hosts: []string{"scores"}}, "scores"},
		{"app name without a domain", AppConfig{Name: "scoreboard"}, "scoreboard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailscaleServiceName(tc.app); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTailscaleServiceNameForHostFollowsTheHostBeingSynced(t *testing.T) {
	// Removing one host must unserve the service that host resolves to, not the
	// one the app's remaining config points at.
	if got := tailscaleServiceNameForHost("scores.corp.ts.net", "scoreboard"); got != "scores" {
		t.Fatalf("got %q, want %q", got, "scores")
	}
	if got := tailscaleServiceNameForHost("", "scoreboard"); got != "scoreboard" {
		t.Fatalf("got %q, want %q", got, "scoreboard")
	}
}
