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
// set, the console must show no such line. With Before set, the first such line must come before
// the first line that holds the Before fragments, which the console must show too.
type Expect struct {
	// What names the expectation in a failure.
	What      string
	Fragments []string
	Absent    bool
	Before    []string
}

// probeChecks are the checks, in the order the probe reports them, that a Machine outlives an
// mvmd restart.
var probeChecks = []string{
	"mvmd-ready", "fencing-armed", "image-pulled", "machine-started", "machine-running", "supervisor-found", "mvmd-found",
	"mvmd-killed", "mvmd-restarted", "supervisor-alive", "supervisor-same-pid", "machine-still-running",
}

// Boot is what a Node's console shows once init has read its configuration, configured the network
// and started mvmd and the stand-in. name is the Node's name in its metadata, such as "node-1".
// It requires one interface and no count of nameservers: the count is a fact of the Node's
// network, which the QEMU tests' network-config fixes at one and a Proxmox network at two.
func Boot(name string) []Expect {
	return append(boot(name, 1), dataDiskMounted, Expect{What: "init started the stand-in", Fragments: []string{"started", "service=standin"}})
}

// boot is what every Node shows, whatever services its configuration names: init's own lines and
// mvmd's start. interfaces is the number of interfaces the Node's network-config gives it.
func boot(name string, interfaces int) []Expect {
	return []Expect{
		{What: "init accepted the configuration", Fragments: []string{"configuration accepted", "node=naru-" + name}},
		{What: "init configured the network", Fragments: []string{"network configured", fmt.Sprintf("interfaces=%d", interfaces)}},
		{What: "init remounted the root read-only", Fragments: []string{"root remounted read-only"}},
		{What: "init started mvmd", Fragments: []string{"started", "service=mvmd"}},
	}
}

// dataDiskMounted is the stand-in's line about the data disk: init logs nothing of its mount, so
// only a Node that runs the stand-in shows it. A Node that runs gaon shows no such line.
var dataDiskMounted = Expect{What: "the data disk is mounted", Fragments: []string{"data disk is mounted", "dir=/var/lib/naru", "fstype=ext4"}}

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

// controlPlaneServices are the gaon services that start only where spec.controlPlane is true, in
// the order init starts them.
var controlPlaneServices = []string{"etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers"}

// Gaon is what the console of a Node that runs gaon shows, in place of what the stand-in and the
// probe make a Node show: init's boot lines, the start of mvmd, the registry and gaon's Node Agent,
// and, by spec.controlPlane, the start of etcd and gaon's control plane or init's skipping of them.
// The registry runs on every Node because the scheduler may bind a Machine to any of them.
// interfaces is the number of interfaces the Node's network-config gives it: one on a Proxmox
// Node, two on the QEMU tests' control-plane Node, which has a second NIC for the host forward.
func Gaon(name string, controlPlane bool, interfaces int) []Expect {
	out := append(boot(name, interfaces),
		Expect{What: "init started the registry", Fragments: []string{"started", "service=registry"}},
		Expect{What: "init started gaon's Node Agent", Fragments: []string{"started", "service=gaon-node-agent"}},
	)
	for _, s := range controlPlaneServices {
		if controlPlane {
			out = append(out, Expect{What: "init started " + s, Fragments: []string{"started", "service=" + s}})
			continue
		}
		out = append(out,
			Expect{What: "init skipped " + s, Fragments: []string{"skipping a control-plane service", "service=" + s}},
			Expect{What: "init never started " + s, Fragments: []string{"started", "service=" + s}, Absent: true},
		)
	}
	return out
}

// Issuance is what the console of a Node whose spec.pki carries a CA shows of init's issuing
// (ADR 0013): the authority written and each named certificate issued, all before init starts
// the first service, and no renewal that failed.
func Issuance(certificates ...string) []Expect {
	firstStart := []string{"started", "service="}
	out := []Expect{{What: "init wrote the authority", Fragments: []string{"authority written"}, Before: firstStart}}
	for _, name := range certificates {
		out = append(out, Expect{
			What: "init issued " + name, Fragments: []string{"certificate issued", "name=" + name}, Before: firstStart,
		})
	}
	return append(out, Expect{What: "no renewal failed", Fragments: []string{"certificate renewal failed"}, Absent: true})
}

// GaonIssued are the certificates the control-plane Node of the QEMU tests' spec.pki test has init
// issue: the API server's, the scheduler's, the controllers' and its own agent's.
var GaonIssued = []string{"apiserver", "scheduler", "controllers", "agent"}

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

// LineIndex is the index of the first line of console that holds every fragment in order, or -1.
func LineIndex(console string, fragments []string) int {
	for i, line := range strings.Split(console, "\n") {
		if lineHas(line, fragments) {
			return i
		}
	}
	return -1
}

// Met reports whether console satisfies e.
func (e Expect) Met(console string) bool {
	at := LineIndex(console, e.Fragments)
	if e.Before == nil || e.Absent {
		return (at >= 0) != e.Absent
	}
	next := LineIndex(console, e.Before)
	return at >= 0 && next >= 0 && at < next
}

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
	if f.Expect.Before != nil {
		return fmt.Sprintf("%s: %s: no line with %q before the first line with %q", f.Node, f.Expect.What, f.Expect.Fragments, f.Expect.Before)
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
