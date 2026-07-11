package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/local/agent-beacon/internal/registry"
	"github.com/local/agent-beacon/pkg/protocol"
)

func newTestServer(t *testing.T, ttl time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Config{AgentToken: "test-psk", HeartbeatTTL: ttl}, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestHealth(t *testing.T) {
	_, ts := newTestServer(t, time.Second)
	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
}

func TestLoginInfoDefaultsToPassword(t *testing.T) {
	_, ts := newTestServer(t, time.Second)
	resp, err := http.Get(ts.URL + "/api/v1/login-info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var li loginInfo
	if err := json.NewDecoder(resp.Body).Decode(&li); err != nil {
		t.Fatal(err)
	}
	if li.Provider != "password" {
		t.Fatalf("provider = %q, want password", li.Provider)
	}
	if li.LogoutURL != "/api/v1/logout" {
		t.Fatalf("logout_url = %q, want /api/v1/logout", li.LogoutURL)
	}
}

func TestAgentAuthRejectsBadToken(t *testing.T) {
	_, ts := newTestServer(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := wsURL(ts.URL) + "/api/v1/agent/connect?session_id=s1"
	_, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail with bad token")
	}
}

func TestHeartbeatAppearsThenGoesStale(t *testing.T) {
	ttl := 150 * time.Millisecond
	s, ts := newTestServer(t, ttl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	url := wsURL(ts.URL) + "/api/v1/agent/connect?session_id=sess-1"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer test-psk"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	hb := protocol.Frame{
		Type:      protocol.FrameHeartbeat,
		SessionID: "sess-1",
		Heartbeat: &protocol.Heartbeat{
			Device: "laptop", Command: "claude", State: protocol.StateRunning, SentAt: time.Now(),
		},
	}
	data, _ := json.Marshal(hb)
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hb: %v", err)
	}

	// Wait for the heartbeat to be ingested.
	waitFor(t, time.Second, func() bool {
		for _, g := range s.reg.Snapshot() {
			if g.Device == "laptop" && len(g.Sessions) == 1 {
				return g.Sessions[0].State == protocol.StateRunning
			}
		}
		return false
	})

	// Stop sending; after TTL the session must be marked stale.
	waitFor(t, 2*time.Second, func() bool {
		for _, g := range s.reg.Snapshot() {
			if g.Device == "laptop" && len(g.Sessions) == 1 {
				return g.Sessions[0].State == protocol.StateStale
			}
		}
		return false
	})
}

func TestRegistryGroupsByDevice(t *testing.T) {
	r := registry.New(time.Minute)
	noop := func(protocol.Frame) error { return nil }
	r.Register("a", noop)
	r.Register("b", noop)
	r.Heartbeat("a", protocol.Heartbeat{Device: "dev", State: protocol.StateRunning})
	r.Heartbeat("b", protocol.Heartbeat{Device: "dev", State: protocol.StateRunning})
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("groups = %d, want 1", len(snap))
	}
	if len(snap[0].Sessions) != 2 {
		t.Fatalf("sessions in group = %d, want 2", len(snap[0].Sessions))
	}
}

func wsURL(httpURL string) string {
	return "ws" + httpURL[len("http"):]
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
