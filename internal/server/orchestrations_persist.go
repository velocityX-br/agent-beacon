package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/agent-beacon/internal/orchestrator"
)

// persistedRun is the on-disk shape of a finished orchestration run. Only
// terminal runs (done/error) are written; the live event stream is never
// persisted — the UI's report panel depends solely on the final Report, so a
// reloaded run renders identically to a live one that has just finished.
type persistedRun struct {
	ID        string              `json:"id"`
	Task      string              `json:"task"`
	Repo      string              `json:"repo"`
	Status    runStatus           `json:"status"`
	StartedAt time.Time           `json:"started_at"`
	Err       string              `json:"error,omitempty"`
	ReportSet bool                `json:"report_set"`
	Report    orchestrator.Report `json:"report"`
}

// persistPath returns the JSON file path for a run id under dir, or "" when no
// persistence directory is configured. The id is a hex/timestamp slug produced
// by newRunID (no path separators), so it is safe as a filename; we defensively
// reject anything that isn't a plain base name.
func persistPath(dir, id string) string {
	if dir == "" || id == "" {
		return ""
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return ""
	}
	return filepath.Join(dir, id+".json")
}

// persist writes a finished run to disk as JSON. It is best-effort: any error
// is returned for logging but never blocks the run's completion. The write is
// atomic (temp file + rename) so a crash mid-write can't leave a truncated file
// that would fail to reload.
func (s *orchStore) persist(r *orchRun) error {
	path := persistPath(s.persistDir, r.ID)
	if path == "" {
		return nil
	}
	r.mu.RLock()
	pr := persistedRun{
		ID:        r.ID,
		Task:      r.Task,
		Repo:      r.Repo,
		Status:    r.status,
		StartedAt: r.StartedAt,
		Err:       r.err,
		ReportSet: r.reportSet,
		Report:    r.report,
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.persistDir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadPersisted scans dir for *.json run files and returns reconstructed
// orchRuns keyed by id. Runs are rehydrated as already-finished (done=true,
// no live subscribers) so the list/detail endpoints and events WS treat them
// identically to a run that completed in this process. Corrupt or unreadable
// files are skipped rather than failing startup.
func loadPersisted(dir string) map[string]*orchRun {
	runs := make(map[string]*orchRun)
	if dir == "" {
		return runs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return runs // missing dir on first run is fine
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var pr persistedRun
		if json.Unmarshal(data, &pr) != nil || pr.ID == "" {
			continue
		}
		runs[pr.ID] = &orchRun{
			ID:          pr.ID,
			Task:        pr.Task,
			Repo:        pr.Repo,
			StartedAt:   pr.StartedAt,
			status:      pr.Status,
			report:      pr.Report,
			reportSet:   pr.ReportSet,
			err:         pr.Err,
			subscribers: make(map[int]chan orchestrator.Event),
			pending:     make(map[string]chan orchestrator.Decision),
			done:        true,
		}
	}
	return runs
}
