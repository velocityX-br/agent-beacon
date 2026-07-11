package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/auth"
)

// newAuthServer builds a server with the given config and an httptest front.
func newAuthServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.HeartbeatTTL == 0 {
		cfg.HeartbeatTTL = time.Second
	}
	s := New(cfg, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestPasswordLoginFlow(t *testing.T) {
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	ts := newAuthServer(t, Config{
		AgentToken:   "psk",
		AuthProvider: string(auth.ModePassword),
		Password:     auth.NewPasswordChecker(hash),
	})

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Unauthenticated sessions call is 401.
	if code := getStatus(t, client, ts.URL+"/api/v1/sessions"); code != http.StatusUnauthorized {
		t.Fatalf("pre-login sessions status = %d, want 401", code)
	}

	// Wrong password is rejected.
	if code := postJSON(t, client, ts.URL+"/api/v1/login", `{"password":"nope"}`); code != http.StatusUnauthorized {
		t.Fatalf("bad-password login status = %d, want 401", code)
	}

	// Correct password issues a cookie.
	if code := postJSON(t, client, ts.URL+"/api/v1/login", `{"password":"s3cret"}`); code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", code)
	}

	// Now sessions is accessible with the cookie.
	if code := getStatus(t, client, ts.URL+"/api/v1/sessions"); code != http.StatusOK {
		t.Fatalf("post-login sessions status = %d, want 200", code)
	}

	// Logout revokes the cookie -> sessions is 401 again.
	if code := postJSON(t, client, ts.URL+"/api/v1/logout", ``); code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", code)
	}
	if code := getStatus(t, client, ts.URL+"/api/v1/sessions"); code != http.StatusUnauthorized {
		t.Fatalf("post-logout sessions status = %d, want 401", code)
	}
}

func TestProxyHeaderAuth(t *testing.T) {
	ts := newAuthServer(t, Config{
		AgentToken:   "psk",
		AuthProvider: string(auth.ModeProxyHeader),
		ProxyHeader:  auth.NewProxyHeaderAuth("X-Forwarded-User", []string{"alice@example.com"}),
	})
	client := &http.Client{}

	// Missing header -> 401.
	if code := getStatus(t, client, ts.URL+"/api/v1/sessions"); code != http.StatusUnauthorized {
		t.Fatalf("no-header status = %d, want 401", code)
	}
	// Disallowed subject -> 401.
	if code := getStatusWithHeader(t, client, ts.URL+"/api/v1/sessions", "X-Forwarded-User", "eve@example.com"); code != http.StatusUnauthorized {
		t.Fatalf("disallowed status = %d, want 401", code)
	}
	// Allowed subject -> 200.
	if code := getStatusWithHeader(t, client, ts.URL+"/api/v1/sessions", "X-Forwarded-User", "alice@example.com"); code != http.StatusOK {
		t.Fatalf("allowed status = %d, want 200", code)
	}
}

func TestNoneAuthIsOpen(t *testing.T) {
	ts := newAuthServer(t, Config{AgentToken: "psk", AuthProvider: string(auth.ModeNone)})
	if code := getStatus(t, &http.Client{}, ts.URL+"/api/v1/sessions"); code != http.StatusOK {
		t.Fatalf("none-auth sessions status = %d, want 200", code)
	}
}

func TestOIDCLoginInfoAdvertisesStartURL(t *testing.T) {
	ts := newAuthServer(t, Config{AgentToken: "psk", AuthProvider: string(auth.ModeOIDC)})
	resp, err := http.Get(ts.URL + "/api/v1/login-info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var li loginInfo
	if err := json.NewDecoder(resp.Body).Decode(&li); err != nil {
		t.Fatal(err)
	}
	if li.Provider != "oidc" {
		t.Fatalf("provider = %q, want oidc", li.Provider)
	}
	if li.StartURL != "/api/v1/auth/start" {
		t.Fatalf("start_url = %q, want /api/v1/auth/start", li.StartURL)
	}
}

// --- helpers ---------------------------------------------------------------

func getStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func getStatusWithHeader(t *testing.T, c *http.Client, url, hk, hv string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(hk, hv)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func postJSON(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	resp, err := c.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
