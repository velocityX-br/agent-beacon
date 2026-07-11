//go:build linux

package monitor

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listProcesses reads /proc directly (no ps exec needed on linux).
func listProcesses() ([]rawProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []rawProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		cmdline := readCmdline(pid)
		if cmdline == "" {
			continue // kernel thread or gone
		}
		procs = append(procs, rawProc{Pid: pid, PPid: readPPid(pid), Cmdline: cmdline})
	}
	return procs, nil
}

// readCmdline joins the NUL-separated /proc/<pid>/cmdline into a space string.
func readCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	data = bytes.TrimRight(data, "\x00")
	return strings.TrimSpace(string(bytes.ReplaceAll(data, []byte{0}, []byte{' '})))
}

// readPPid extracts the parent pid from /proc/<pid>/stat. The comm field can
// contain spaces/parens, so parse from the closing ')' of the comm field.
func readPPid(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	s := string(data)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		rest := strings.Fields(s[i+2:]) // state, ppid, ...
		if len(rest) >= 2 {
			ppid, _ := strconv.Atoi(rest[1])
			return ppid
		}
	}
	return 0
}

// processCWD reads the /proc/<pid>/cwd symlink.
func processCWD(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
}
