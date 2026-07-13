package registry

import (
	"bytes"
	"testing"

	"github.com/local/agent-beacon/pkg/protocol"
)

func newTestSession() *Session {
	return &Session{
		ID:          "test",
		send:        func(protocol.Frame) error { return nil },
		subscribers: make(map[int]chan []byte),
		termSizes:   make(map[int]protocol.ResizeMsg),
	}
}

// TestScrollbackReplayed verifies output published before a browser subscribes
// is captured and replayed as the subscribe backlog.
func TestScrollbackReplayed(t *testing.T) {
	s := newTestSession()
	s.PublishOutput([]byte("hello "))
	s.PublishOutput([]byte("world"))

	_, backlog, _, unsub := s.Subscribe()
	defer unsub()
	if got := string(backlog); got != "hello world" {
		t.Fatalf("backlog = %q, want %q", got, "hello world")
	}
}

// TestScrollbackCapped verifies the scrollback keeps only the tail once it
// exceeds maxScrollback.
func TestScrollbackCapped(t *testing.T) {
	s := newTestSession()
	total := maxScrollback + 10*1024
	// Write in chunks; the last maxScrollback bytes should be retained.
	buf := make([]byte, total)
	for i := range buf {
		buf[i] = byte('A' + (i % 26))
	}
	s.PublishOutput(buf)

	_, backlog, _, unsub := s.Subscribe()
	defer unsub()
	if len(backlog) != maxScrollback {
		t.Fatalf("backlog len = %d, want %d", len(backlog), maxScrollback)
	}
	want := buf[total-maxScrollback:]
	if !bytes.Equal(backlog, want) {
		t.Fatalf("backlog is not the retained tail")
	}
}

// TestSizeNegotiationMinimum verifies the negotiated PTY size is the
// element-wise minimum across attached browsers, and that detaching one browser
// lets the size grow back to the remaining browser's minimum.
func TestSizeNegotiationMinimum(t *testing.T) {
	s := newTestSession()

	if rows, cols, changed := s.SetSubscriberSize(1, 40, 100); !changed || rows != 40 || cols != 100 {
		t.Fatalf("first size = (%d,%d,%v), want (40,100,true)", rows, cols, changed)
	}
	if rows, cols, changed := s.SetSubscriberSize(2, 30, 80); !changed || rows != 30 || cols != 80 {
		t.Fatalf("second size = (%d,%d,%v), want (30,80,true)", rows, cols, changed)
	}

	// Removing sub 2's entry should recompute back up to sub 1's size.
	s.mu.Lock()
	delete(s.termSizes, 2)
	rows, cols, changed := s.recomputeSizeLocked()
	s.mu.Unlock()
	if !changed || rows != 40 || cols != 100 {
		t.Fatalf("after removing sub 2 = (%d,%d,%v), want (40,100,true)", rows, cols, changed)
	}
}

// TestUnsubscribeRecomputesSize verifies the Subscribe unsubscribe closure
// removes the size entry so a remaining browser's minimum takes effect.
func TestUnsubscribeRecomputesSize(t *testing.T) {
	var resizes []protocol.ResizeMsg
	s := newTestSession()
	s.send = func(f protocol.Frame) error {
		if f.Type == protocol.FrameResize && f.Resize != nil {
			resizes = append(resizes, *f.Resize)
		}
		return nil
	}

	sub1, _, _, unsub1 := s.Subscribe()
	sub2, _, _, unsub2 := s.Subscribe()
	_ = unsub1

	if _, _, changed := s.SetSubscriberSize(sub1, 40, 100); !changed {
		t.Fatal("sub1 size should change")
	}
	if _, _, changed := s.SetSubscriberSize(sub2, 30, 80); !changed {
		t.Fatal("sub2 size should change")
	}

	// Detaching the smaller browser (sub2) should push a resize back up to 40x100.
	unsub2()
	if len(resizes) == 0 {
		t.Fatal("expected a resize after detach")
	}
	last := resizes[len(resizes)-1]
	if last.Rows != 40 || last.Cols != 100 {
		t.Fatalf("resize after detach = (%d,%d), want (40,100)", last.Rows, last.Cols)
	}
}

// TestBrowserDrivesSizeExcludingLocal verifies the local terminal (iTerm2) size
// is EXCLUDED from negotiation whenever a browser is attached — the browser
// drives the PTY size — and is used only when no browser is attached. This is
// what lets the dashboard fill its panel even when the local iTerm2 window is
// small.
func TestBrowserDrivesSizeExcludingLocal(t *testing.T) {
	s := newTestSession()

	// A browser attaches at 40x100.
	if rows, cols, changed := s.SetSubscriberSize(1, 40, 100); !changed || rows != 40 || cols != 100 {
		t.Fatalf("browser size = (%d,%d,%v), want (40,100,true)", rows, cols, changed)
	}

	// A larger local size is ignored while a browser is attached (browser drives).
	if rows, cols, changed := s.SetLocalSize(50, 120); changed || rows != 40 || cols != 100 {
		t.Fatalf("larger local = (%d,%d,%v), want (40,100,false)", rows, cols, changed)
	}

	// A SMALLER local size must ALSO be ignored now: a small iTerm2 no longer
	// caps the browser terminal (this is the behavior change).
	if rows, cols, changed := s.SetLocalSize(30, 80); changed || rows != 40 || cols != 100 {
		t.Fatalf("smaller local = (%d,%d,%v), want (40,100,false) — local must not cap the browser", rows, cols, changed)
	}

	// Local-only case: with no browsers, the local size IS the negotiated size,
	// so a purely local managed session still matches the physical terminal.
	s2 := newTestSession()
	if rows, cols, changed := s2.SetLocalSize(45, 110); !changed || rows != 45 || cols != 110 {
		t.Fatalf("local-only = (%d,%d,%v), want (45,110,true)", rows, cols, changed)
	}
}
