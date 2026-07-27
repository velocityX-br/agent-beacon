package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/local/agent-beacon/pkg/protocol"
)

// recoverRecord is the on-disk shape of one recoverable managed session. It
// captures just enough to relaunch the session in the same workspace with its
// prior conversation reloaded (via `claude --continue`) after a reboot. The
// live PTY stream, scrollback, and session id are intentionally NOT persisted:
// recovery re-spawns a fresh session in the same cwd, and claude reloads the
// most recent transcript for that directory.
type recoverRecord struct {
	RecoveryID string    `json:"recovery_id"`
	Device     string    `json:"device"`
	CWD        string    `json:"cwd"`
	Command    string    `json:"command"`
	Branch     string    `json:"branch,omitempty"` // diagnostic only
	SavedAt    time.Time `json:"saved_at"`
}

// recoveryID derives a stable, filename-safe id for a workspace (device+cwd).
// Session ids are time-based and change on every spawn, so keying records by
// workspace means a re-spawn of the same directory overwrites the same file
// rather than accumulating stale entries. The hex-only sha256 prefix can never
// contain a path separator, so it is always a safe base name.
func recoveryID(device, cwd string) string {
	sum := sha256.Sum256([]byte(device + "\x00" + cwd))
	return hex.EncodeToString(sum[:])[:16]
}

// recoverPath returns the JSON file path for a recovery id under dir, or ""
// when no directory is configured or the id is not a plain base name. The id
// comes from recoveryID (hex only), so this defensively rejects anything with a
// separator, mirroring persistPath's filename-safety check.
func recoverPath(dir, id string) string {
	if dir == "" || id == "" {
		return ""
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return ""
	}
	return filepath.Join(dir, id+".json")
}

// recoverStore persists recovery records for managed sessions and serves the
// pending set back after a restart. Every method is a no-op when dir == "", so
// disabling recovery is simply "no directory configured". Writes are atomic
// (temp + rename) and throttled per record so a chatty heartbeat stream does
// not rewrite the same file on every tick.
type recoverStore struct {
	dir       string
	minGap    time.Duration
	mu        sync.Mutex
	pending   map[string]recoverRecord // id -> record (loaded at boot, drained on recovery)
	lastSaved map[string]time.Time     // id -> last write time (throttle)
}

// newRecoverStore builds a store rooted at dir, preloading any records left on
// disk from a prior process so they can be recovered as agents reconnect. An
// empty dir yields a disabled store (all methods no-op).
func newRecoverStore(dir string) *recoverStore {
	return &recoverStore{
		dir:       dir,
		minGap:    10 * time.Second,
		pending:   loadRecoverRecords(dir),
		lastSaved: make(map[string]time.Time),
	}
}

// saveHeartbeat persists (or refreshes) the recovery record for a managed
// session from its latest heartbeat. It is best-effort and throttled: a record
// is rewritten at most once per minGap, so the steady heartbeat stream costs
// one write per ~10s per workspace. No-op when disabled or when the heartbeat
// carries no cwd (nothing to relaunch into).
func (s *recoverStore) saveHeartbeat(hb protocol.Heartbeat) {
	if s.dir == "" || hb.CWD == "" {
		return
	}
	id := recoveryID(hb.Device, hb.CWD)
	now := time.Now()

	s.mu.Lock()
	if last, ok := s.lastSaved[id]; ok && now.Sub(last) < s.minGap {
		s.mu.Unlock()
		return
	}
	rec := recoverRecord{
		RecoveryID: id,
		Device:     hb.Device,
		CWD:        hb.CWD,
		Command:    hb.Command,
		Branch:     hb.Branch,
		SavedAt:    now,
	}
	s.lastSaved[id] = now
	// Keep the in-memory pending set current so a recovery triggered within the
	// same process (e.g. a monitor that connects after the session started)
	// sees this workspace. On a fresh start pending is seeded from disk instead.
	s.pending[id] = rec
	s.mu.Unlock()

	_ = s.write(rec)
}

// write atomically persists one record as JSON (temp file + rename) so a crash
// mid-write can't leave a truncated file that would fail to reload. No-op when
// the store is disabled.
func (s *recoverStore) write(rec recoverRecord) error {
	path := recoverPath(s.dir, rec.RecoveryID)
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeByDeviceCwd deletes the recovery record for a workspace and drops it
// from the in-memory maps. Called when the dashboard kill button ends a session
// deliberately — the operator wants it gone, not recovered. A missing file (or
// disabled store) is not an error.
func (s *recoverStore) removeByDeviceCwd(device, cwd string) error {
	if s.dir == "" || cwd == "" {
		return nil
	}
	id := recoveryID(device, cwd)
	s.mu.Lock()
	delete(s.pending, id)
	delete(s.lastSaved, id)
	s.mu.Unlock()

	path := recoverPath(s.dir, id)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// takePending atomically removes and returns every pending record for a device.
// Draining guarantees at most one recovery spawn per record per server lifetime
// (no retry loop). It deletes from the in-memory pending map only — the on-disk
// file is left in place because the freshly re-spawned session re-persists it
// via saveHeartbeat; a workspace that fails to relaunch simply leaves a stale
// file, which is harmless and overwritten on the next successful run.
func (s *recoverStore) takePending(device string) []recoverRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recoverRecord
	for id, rec := range s.pending {
		if rec.Device == device {
			out = append(out, rec)
			delete(s.pending, id)
		}
	}
	return out
}

// loadRecoverRecords scans dir for *.json recovery files and returns them keyed
// by recovery id. Corrupt, empty, or unreadable files are skipped rather than
// failing startup, matching loadPersisted. A missing dir on first run is fine.
func loadRecoverRecords(dir string) map[string]recoverRecord {
	recs := make(map[string]recoverRecord)
	if dir == "" {
		return recs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return recs
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec recoverRecord
		if json.Unmarshal(data, &rec) != nil || rec.RecoveryID == "" || rec.CWD == "" {
			continue
		}
		recs[rec.RecoveryID] = rec
	}
	return recs
}
