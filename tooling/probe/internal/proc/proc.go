// Package proc reads what the probe needs of a Node's /proc and cgroup
// tree: which processes a cgroup holds, what each is called, and when it
// started. A process is told from a later one that reuses its PID by the
// start time.
package proc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Process names one process by what a PID reuse cannot repeat.
type Process struct {
	PID int
	// Start is the process's start time in clock ticks since boot (field 22 of /proc/PID/stat).
	Start uint64
	Comm  string
}

// Reader reads a /proc tree and a cgroup tree; both roots are fields so that
// tests can point them at a fake.
type Reader struct {
	Proc string
}

// Members returns the processes the cgroup directory lists in cgroup.procs.
func (r Reader) Members(cgroupDir string) ([]Process, error) {
	f, err := os.Open(filepath.Join(cgroupDir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Process
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cgroupDir, err)
		}
		p, err := r.Read(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue // it ended between the two reads
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// Find returns the one process of the cgroup called comm. It fails when there
// is none or more than one, since the probe cannot then say which it means.
func (r Reader) Find(cgroupDir, comm string) (Process, error) {
	members, err := r.Members(cgroupDir)
	if err != nil {
		return Process{}, err
	}
	var found []Process
	for _, m := range members {
		if m.Comm == comm {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return Process{}, fmt.Errorf("no process called %q in %s", comm, cgroupDir)
	default:
		return Process{}, fmt.Errorf("%d processes called %q in %s", len(found), comm, cgroupDir)
	}
}

// Read returns the process with the PID.
func (r Reader) Read(pid int) (Process, error) {
	b, err := os.ReadFile(filepath.Join(r.Proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return Process{}, err
	}
	return parseStat(pid, string(b))
}

// Alive reports whether p is still running: its PID answers and the start
// time is the same. A zombie is not alive.
func (r Reader) Alive(p Process) bool {
	b, err := os.ReadFile(filepath.Join(r.Proc, strconv.Itoa(p.PID), "stat"))
	if err != nil {
		return false
	}
	now, err := parseStat(p.PID, string(b))
	if err != nil || now.Start != p.Start || now.Comm != p.Comm {
		return false
	}
	return stateOf(string(b)) != 'Z'
}

// parseStat reads comm and the start time out of a /proc/PID/stat line. comm
// is in parentheses and may itself hold spaces and parentheses, so the fields
// are counted from the last closing one.
func parseStat(pid int, stat string) (Process, error) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return Process{}, fmt.Errorf("pid %d: stat has no command name: %q", pid, stat)
	}
	// After ") " the fields are numbered from 3 (state).
	fields := strings.Fields(stat[end+1:])
	const startField = 22
	if len(fields) < startField-2 {
		return Process{}, fmt.Errorf("pid %d: stat has %d fields after the name", pid, len(fields))
	}
	start, err := strconv.ParseUint(fields[startField-3], 10, 64)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: start time: %w", pid, err)
	}
	return Process{PID: pid, Start: start, Comm: stat[open+1 : end]}, nil
}

// stateOf is the state letter of a stat line, or 0.
func stateOf(stat string) byte {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return 0
	}
	return stat[end+2]
}
