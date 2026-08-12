package server

import (
	"bytes"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/agent-beacon/internal/auth"
)

// TestLoginLimiterLockout drives the limiter directly with a fake clock: it
// should allow up to the threshold of failures, then lock out with a growing
// backoff, and clear on success.
func TestLoginLimiterLockout(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	const ip = "203.0.113.7"

	// Up to (threshold-1) failures still allow the next attempt.
	for i := 0; i < l.threshold-1; i++ {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("attempt %d unexpectedly blocked before threshold", i)
		}
		l.recordFailure(ip)
	}
	// The threshold-th failure triggers a lockout.
	if ok, _ := l.allow(ip); !ok {
		t.Fatal("attempt at threshold-1 failures should still be allowed")
	}
	l.recordFailure(ip) // this is the threshold-th failure
	ok, wait := l.allow(ip)
	if ok {
		t.Fatal("expected lockout after threshold failures, but allow() returned true")
	}
	if wait < l.baseBackoff {
		t.Fatalf("lockout wait = %v, want >= baseBackoff %v", wait, l.baseBackoff)
	}

	// Still locked out just before the window elapses.
	now = now.Add(wait - time.Millisecond)
	if ok, _ := l.allow(ip); ok {
		t.Fatal("should still be locked just before backoff elapses")
	}
	// Allowed once the backoff elapses.
	now = now.Add(2 * time.Millisecond)
	if ok, _ := l.allow(ip); !ok {
		t.Fatal("should be allowed after backoff elapses")
	}

	// A success clears state entirely.
	l.recordSuccess(ip)
	if ok, _ := l.allow(ip); !ok {
		t.Fatal("success should reset limiter state")
	}
	l.mu.Lock()
	_, present := l.attempts[ip]
	l.mu.Unlock()
	if present {
		t.Fatal("recordSuccess should delete the IP's attempt state")
	}
}

// TestLoginLimiterBackoffGrows checks that successive lockouts past the
// threshold widen the backoff (exponential), capped at maxBackoff.
func TestLoginLimiterBackoffGrows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	const ip = "198.51.100.9"

	var prev time.Duration
	for i := 0; i < l.threshold+3; i++ {
		l.recordFailure(ip)
	}
	_, wait := l.allow(ip)
	prev = wait
	if prev <= 0 {
		t.Fatalf("expected a lockout window, got %v", prev)
	}
	if prev > l.maxBackoff {
		t.Fatalf("backoff %v exceeded cap %v", prev, l.maxBackoff)
	}
}

// TestLoginLimiterWindowForget confirms an idle IP is forgotten after window.
func TestLoginLimiterWindowForget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	const ip = "192.0.2.1"

	l.recordFailure(ip)
	now = now.Add(l.window + time.Second)
	if ok, _ := l.allow(ip); !ok {
		t.Fatal("idle IP past the window should be allowed")
	}
	l.mu.Lock()
	_, present := l.attempts[ip]
	l.mu.Unlock()
	if present {
		t.Fatal("stale IP state should be reaped by allow()")
	}
}

// TestClientIPPrefersXFF verifies the throttle key extraction: X-Forwarded-For
// (left-most) wins, else the RemoteAddr host.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		xff    string
		remote string
		want   string
	}{
		{"xff single", "203.0.113.5", "10.0.0.1:1234", "203.0.113.5"},
		{"xff chain", "203.0.113.5, 70.41.3.18, 150.172.238.178", "10.0.0.1:1234", "203.0.113.5"},
		{"xff spaced", "  203.0.113.5  ", "10.0.0.1:1234", "203.0.113.5"},
		{"remote only", "", "10.0.0.1:1234", "10.0.0.1"},
		{"remote no port", "", "unixsocket", "unixsocket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/api/v1/login", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoginRateLimitOverHTTP exercises the wired-in limiter through the real
// handler: after enough bad-password attempts from the same client, the login
// endpoint returns 429 with a Retry-After header instead of 401.
func TestLoginRateLimitOverHTTP(t *testing.T) {
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		AgentToken:   "psk",
		AuthProvider: string(auth.ModePassword),
		Password:     auth.NewPasswordChecker(hash),
	}, nil)
	// Pin the clock so the test is deterministic and never actually waits.
	s.loginRate.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// The first `threshold` bad attempts are 401 (invalid password).
	for i := 0; i < s.loginRate.threshold; i++ {
		if code := postJSON(t, client, ts.URL+"/api/v1/login", `{"password":"nope"}`); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i, code)
		}
	}
	// The next attempt is throttled: 429.
	code, hdr := postJSONWithHeaders(t, client, ts.URL+"/api/v1/login", `{"password":"nope"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("throttled status = %d, want 429", code)
	}
	if hdr.Get("Retry-After") == "" {
		t.Fatal("throttled response missing Retry-After header")
	}
	// Even the correct password is throttled while locked out.
	if code := postJSON(t, client, ts.URL+"/api/v1/login", `{"password":"s3cret"}`); code != http.StatusTooManyRequests {
		t.Fatalf("correct-password-while-locked status = %d, want 429", code)
	}
}

// postJSONWithHeaders is like postJSON but also returns the response headers so
// a test can assert on Retry-After.
func postJSONWithHeaders(t *testing.T, c *http.Client, url, body string) (int, http.Header) {
	t.Helper()
	resp, err := c.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header
}
