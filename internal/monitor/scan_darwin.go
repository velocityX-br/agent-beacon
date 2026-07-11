//go:build darwin

package monitor

import (
	"os/exec"
	"strconv"
	"strings"
)

// listProcesses shells out to ps for the full process table. macOS has no /proc,
// so ps is the portable source. We request only pid, ppid and the full argv
// (-o args= is not truncated, unlike comm), then parse the fixed leading columns.
func listProcesses() ([]rawProc, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil, err
	}
	var procs []rawProc
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		cmdline := strings.Join(fields[2:], " ")
		procs = append(procs, rawProc{Pid: pid, PPid: ppid, Cmdline: cmdline})
	}
	return procs, nil
}

// processCWD returns a process's working directory via lsof. Output form:
//
//	p<pid>
//	fcwd
//	n<path>
func processCWD(pid int) (string, error) {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimSpace(line[1:]), nil
		}
	}
	return "", nil
}
