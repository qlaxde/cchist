package main

import (
	"bufio"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// claudeProcess is one running Claude CLI instance observed via ps. Memory is
// reported in bytes — we convert RSS from kB (ps output) at parse time so all
// arithmetic downstream stays in one unit.
type claudeProcess struct {
	PID       int
	RSSBytes  uint64
	Etime     string // elapsed time since launch, as ps prints it
	Command   string
	SessionID string // best-effort extract from --session-id / --resume argv
}

var sessionArgRE = regexp.MustCompile(`--(?:session-id|resume)[= ]([0-9a-f-]{8,36})`)

// listClaudeProcesses shells out to ps once and parses every line where the
// command contains "claude". We explicitly exclude our own cchist invocation
// so `cchist running` doesn't list itself.
func listClaudeProcesses() ([]claudeProcess, error) {
	cmd := exec.Command("ps", "-eo", "pid=,rss=,etime=,command=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var result []claudeProcess
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Split on whitespace: pid, rss, etime, then command (rest of line).
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		rss, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		etime := fields[2]
		command := strings.Join(fields[3:], " ")
		// Filter: must look like the claude CLI, and must not be us.
		lower := strings.ToLower(command)
		if !strings.Contains(lower, "claude") {
			continue
		}
		if strings.Contains(command, "cchist") {
			continue
		}
		// Heuristics to drop obvious non-Claude-Code processes. Claude's
		// Desktop app also matches "claude"; skip GUI app bundles.
		if strings.Contains(command, ".app/Contents/") {
			continue
		}
		proc := claudeProcess{
			PID:      pid,
			RSSBytes: rss * 1024,
			Etime:    etime,
			Command:  command,
		}
		if m := sessionArgRE.FindStringSubmatch(command); len(m) == 2 {
			proc.SessionID = m[1]
		}
		result = append(result, proc)
	}
	return result, sc.Err()
}

// terminate sends SIGTERM, waits up to the timeout for the process to die,
// then escalates to SIGKILL. Returns true if the process is gone (either
// because of us or on its own).
func terminate(pid int, timeout time.Duration) bool {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		// ESRCH means it already exited — treat as success.
		return err == syscall.ESRCH
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true // no such process
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// Give the kernel a moment, then check.
	time.Sleep(200 * time.Millisecond)
	return syscall.Kill(pid, 0) != nil
}
