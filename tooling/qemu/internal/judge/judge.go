// Package judge says what the console of a naru Node must show, and checks a captured console
// against it. The QEMU tests wait for these expectations on live consoles, and `make
// judge-serial` checks them on the serial logs of two Nodes of any host, such as Proxmox
// guests, so one list decides both.
package judge

import (
	"fmt"
	"regexp"
	"strings"
)

// Expect is one thing a console must show: one line holding every fragment, in order. With Absent
// set, the console must show no such line.
type Expect struct {
	// What names the expectation in a failure.
	What      string
	Fragments []string
	Absent    bool
}

// probeChecks are the checks, in the order the probe reports them, that a Machine outlives an
// mvmd restart.
var probeChecks = []string{
	"mvmd-ready", "fencing-armed", "image-pulled", "machine-started", "machine-running", "supervisor-found", "mvmd-found",
	"mvmd-killed", "mvmd-restarted", "supervisor-alive", "supervisor-same-pid", "machine-still-running",
}

// Boot is what a Node's console shows once init has read its configuration, configured the network
// and started mvmd and the stand-in. name is the Node's name in its metadata, such as "node-1".
func Boot(name string) []Expect {
	return []Expect{
		{What: "init accepted the configuration", Fragments: []string{"configuration accepted", "node=naru-" + name}},
		{What: "init configured the network", Fragments: []string{"network configured", "interfaces=1", "nameservers=1"}},
		{What: "init remounted the root read-only", Fragments: []string{"root remounted read-only"}},
		{What: "the data disk is mounted", Fragments: []string{"data disk is mounted", "dir=/var/lib/mvmd", "fstype=ext4"}},
		{What: "init started mvmd", Fragments: []string{"started", "service=mvmd"}},
		{What: "init started the stand-in", Fragments: []string{"started", "service=standin"}},
	}
}

// Services is what a Node's console shows once every service runs, mvmd answers and the stand-in
// reached the stand-in of the other Node.
func Services(name string) []Expect {
	return append(Boot(name),
		Expect{What: "init started the registry", Fragments: []string{"started", "service=registry"}},
		Expect{What: "mvmd answers on its socket", Fragments: []string{"socket accepts connections", "path=/run/mvmd.sock"}},
		Expect{What: "the stand-in reached the other Node's stand-in", Fragments: []string{"peer accepts connections"}},
	)
}

// ControlPlane is what a Node's console shows of the control service: started where
// spec.controlPlane is true, skipped and never started where it is false.
func ControlPlane(controlPlane bool) []Expect {
	if controlPlane {
		return []Expect{
			{What: "init started the control service", Fragments: []string{"started", "service=control"}},
			{What: "init skipped no control-plane service", Fragments: []string{"skipping a control-plane service"}, Absent: true},
		}
	}
	return []Expect{
		{What: "init skipped the control service", Fragments: []string{"skipping a control-plane service", "service=control"}},
		{What: "init never started the control service", Fragments: []string{"started", "service=control"}, Absent: true},
	}
}

// Probe is what the console of the Node that runs the probe shows: every check ok, no check
// failed, the probe finished, and init restarted mvmd.
func Probe() []Expect {
	var out []Expect
	for _, c := range probeChecks {
		out = append(out, Expect{What: "the probe reports " + c, Fragments: []string{"PROBE " + c + " ok"}})
	}
	return append(out,
		Expect{What: "the probe finished", Fragments: []string{"PROBE finished"}},
		Expect{What: "no probe check failed", Fragments: []string{"PROBE ", " fail "}, Absent: true},
		Expect{What: "init restarted mvmd", Fragments: []string{"restarting", "service=mvmd"}},
	)
}

// Node is what one Node's console must show, in the cluster the tests and the release's example
// user-data share: node-1 is the control-plane Node and runs the probe.
func Node(name string, controlPlane, probe bool) []Expect {
	out := append(Services(name), ControlPlane(controlPlane)...)
	if probe {
		out = append(out, Probe()...)
	}
	return out
}

// HasLine reports whether one line of console holds every fragment in order.
func HasLine(console string, fragments []string) bool {
	for _, line := range strings.Split(console, "\n") {
		if lineHas(line, fragments) {
			return true
		}
	}
	return false
}

func lineHas(line string, fragments []string) bool {
	rest := line
	for _, f := range fragments {
		i := strings.Index(rest, f)
		if i < 0 {
			return false
		}
		rest = rest[i+len(f):]
	}
	return true
}

// Met reports whether console satisfies e.
func (e Expect) Met(console string) bool { return HasLine(console, e.Fragments) != e.Absent }

// The escape sequences a terminal capture holds: CSI, OSC and the two-byte escapes.
var ansi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// Log is a captured console, cleaned.
type Log struct {
	// Text is the console, one line per line, with the markers of the re-prints removed.
	Text string
	// Reprints is the number of re-printed blocks seen, counted by their end marker.
	Reprints int
	// Dropped is the most lines a begin marker said init had dropped before it re-printed.
	Dropped int
}

// Clean reads a raw capture of a console: it takes out the terminal's escape sequences and other
// control characters, ends a line at CR, LF or CR LF, and removes the markers of init's re-prints.
// It tolerates a capture that starts in the middle of a line. A line init printed that a re-print
// printed again stays, once per time: an expectation asks whether a line is there, not how often.
func Clean(raw []byte) Log {
	s := ansi.ReplaceAllString(string(raw), "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var l Log
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		line = strings.Map(func(r rune) rune {
			if r < ' ' && r != '\t' {
				return -1
			}
			return r
		}, line)
		var seq, lines, dropped int
		switch {
		case scan(line, "NARU-LOG-BEGIN seq=%d lines=%d dropped=%d", &seq, &lines, &dropped):
			l.Dropped = max(l.Dropped, dropped)
			continue
		case scan(line, "NARU-LOG-END seq=%d", &seq):
			l.Reprints++
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	l.Text = out.String()
	return l
}

// scan reports whether line is the format with its numbers read into args.
func scan(line, format string, args ...any) bool {
	if !strings.HasPrefix(strings.TrimSpace(line), strings.SplitN(format, " ", 2)[0]) {
		return false
	}
	n, err := fmt.Sscanf(strings.TrimSpace(line), format, args...)
	return err == nil && n == len(args)
}

// Failure is an expectation a Node's console did not meet.
type Failure struct {
	Node   string
	Expect Expect
}

func (f Failure) String() string {
	if f.Expect.Absent {
		return fmt.Sprintf("%s: %s: a line with %q is there", f.Node, f.Expect.What, f.Expect.Fragments)
	}
	return fmt.Sprintf("%s: %s: no line with %q", f.Node, f.Expect.What, f.Expect.Fragments)
}

// Check returns the expectations the log does not meet.
func Check(node string, l Log, want []Expect) []Failure {
	var out []Failure
	for _, e := range want {
		if !e.Met(l.Text) {
			out = append(out, Failure{Node: node, Expect: e})
		}
	}
	return out
}
