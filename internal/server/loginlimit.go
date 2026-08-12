package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter throttles password-login attempts per client IP to blunt online
// brute force — the login endpoint is otherwise a bare, internet-reachable POST
// (relevant once the built-in tunnel exposes it publicly). It is intentionally
// small and in-memory: on each failed attempt the IP's failure count grows, and
// once it crosses a threshold the IP is locked out for a backoff window that
// doubles with continued abuse (capped). A success resets the IP's state.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptState

	// threshold is the number of failures allowed before lockout kicks in.
	threshold int
	// baseBackoff is the first lockout window; it doubles per extra failure.
	baseBackoff time.Duration
	// maxBackoff caps the lockout window.
	maxBackoff time.Duration
	// window forgets an IP's failures after this long with no activity, so a
	// benign fat-finger doesn't accrue forever.
	window time.Duration

	now func() time.Time // injectable clock for tests
}

type attemptState struct {
	failures  int
	lockUntil time.Time
	last      time.Time
}

// newLoginLimiter builds a limiter with sane defaults for a single-replica POC:
// 5 failures before lockout, 1s initial backoff doubling to 5m, forgetting an
// idle IP after 15m.
func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		attempts:    make(map[string]*attemptState),
		threshold:   5,
		baseBackoff: 1 * time.Second,
		maxBackoff:  5 * time.Minute,
		window:      15 * time.Minute,
		now:         time.Now,
	}
}

// allow reports whether key (a client IP) may attempt a login now. When locked
// out it returns false and the remaining wait, so the caller can send a
// Retry-After. Expired lockouts and stale windows are cleaned up lazily.
func (l *loginLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st, ok := l.attempts[key]
	if !ok {
		return true, 0
	}
	// Forget stale state so a long-idle IP starts fresh.
	if now.Sub(st.last) > l.window && now.After(st.lockUntil) {
		delete(l.attempts, key)
		return true, 0
	}
	if now.Before(st.lockUntil) {
		return false, st.lockUntil.Sub(now)
	}
	return true, 0
}

// recordFailure increments the failure count for key and, once the threshold is
// crossed, sets/extends the lockout with exponential backoff (capped).
func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.attempts[key]
	if st == nil {
		st = &attemptState{}
		l.attempts[key] = st
	}
	st.failures++
	st.last = now
	if st.failures >= l.threshold {
		// backoff = base * 2^(failures-threshold), capped at maxBackoff.
		backoff := l.baseBackoff
		for i := l.threshold; i < st.failures; i++ {
			backoff *= 2
			if backoff >= l.maxBackoff {
				backoff = l.maxBackoff
				break
			}
		}
		st.lockUntil = now.Add(backoff)
	}
}

// recordSuccess clears any throttling state for key after a valid login.
func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

// clientIP extracts a best-effort client identifier for throttling. It prefers
// the left-most X-Forwarded-For entry when present (the tunnel / reverse proxy
// sets it), falling back to the transport remote address. This is used only for
// rate-limit bucketing, never for authorization, so a spoofed header at worst
// lets an attacker rotate their own bucket — it cannot bypass auth.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
