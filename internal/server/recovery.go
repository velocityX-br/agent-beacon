package server

import (
	"path/filepath"
	"strings"

	"github.com/local/agent-beacon/pkg/protocol"
)

// withContinue appends "--continue" to a claude command so recovery relaunches
// the session with its most recent conversation transcript reloaded. It is a
// no-op for non-claude commands (they get launched verbatim) and idempotent for
// commands that already ask to continue (via --continue or the -c shorthand).
// An empty command defaults to "claude --continue".
func withContinue(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "claude --continue"
	}
	if filepath.Base(fields[0]) != "claude" {
		return command // non-claude worker: relaunch exactly as recorded
	}
	for _, f := range fields {
		if f == "--continue" || f == "-c" {
			return command
		}
	}
	return command + " --continue"
}

// recoverDevice re-spawns each pending recovery record for a device using the
// supplied spawn func (wired by the caller to the device's live monitor WS).
// Recovery must ride on a live agent connection because SendSpawn travels over
// a device WebSocket — the server has no agents at process start, so this is
// triggered when a monitor reconnects after a reboot.
//
// Records are drained (takePending) so each is attempted at most once per
// server lifetime. A workspace that already has a live managed session is
// skipped: that means the server restarted without a reboot (the agent survived
// and reconnected), so re-spawning would duplicate the session. The spawn is
// marked TrustedCwd because the cwd is server-authoritative (persisted from the
// session's own heartbeat), and WorktreeBranch is left empty — the cwd is
// already the workspace/worktree directory, so no new worktree is created.
func (s *Server) recoverDevice(device string, spawn func(protocol.SpawnMsg) error) {
	for _, rec := range s.recover.takePending(device) {
		if s.reg.HasManagedOnDeviceCwd(device, rec.CWD) {
			continue
		}
		m := protocol.SpawnMsg{
			ProjectPath: rec.CWD,
			Command:     withContinue(rec.Command),
			TrustedCwd:  true,
		}
		if err := spawn(m); err != nil {
			s.log.Warn("session recovery spawn failed", "device", device, "cwd", rec.CWD, "err", err)
			continue
		}
		s.log.Info("recovered managed session", "device", device, "cwd", rec.CWD)
	}
}
