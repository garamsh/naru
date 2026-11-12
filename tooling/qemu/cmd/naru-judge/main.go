// Command naru-judge reads the serial logs of two Nodes and exits 0 only when they show what the
// QEMU tests assert; `make judge-serial NODE1=<log> NODE2=<log>` runs it. The logs are raw
// captures, such as Proxmox's termproxy or `qm terminal` writes. node-1 is the control-plane Node
// and, without -gaon, runs the probe; with -gaon the Nodes run gaon's services in place of the
// stand-ins and the probe, and -pki and -node1-interfaces say what node-1's log must show of its
// issuing and its network.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("naru-judge", flag.ContinueOnError)
	fs.SetOutput(errOut)
	node1 := fs.String("node1", "", "serial log of node-1, the control-plane Node that runs the probe")
	node2 := fs.String("node2", "", "serial log of node-2")
	gaon := fs.Bool("gaon", false, "the Nodes run gaon (the release's user-data), not the stand-ins and the probe")
	pki := fs.Bool("pki", false, "with -gaon, node-1 issued its gaon pairs from spec.pki (ADR 0013), and its issuing is judged")
	node1Interfaces := fs.Int("node1-interfaces", 1, "with -gaon, the interfaces node-1's network-config gives it: the QEMU tests' control-plane Node has two, a Proxmox Node one or two")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*pki || *node1Interfaces != 1) && !*gaon {
		fmt.Fprintln(errOut, "naru-judge: -pki and -node1-interfaces are judged only with -gaon")
		return 2
	}
	if *node1Interfaces < 1 {
		fmt.Fprintln(errOut, "naru-judge: -node1-interfaces must be at least 1")
		return 2
	}
	if *node1 == "" || *node2 == "" {
		fmt.Fprintln(errOut, "naru-judge: -node1 and -node2 are required; make judge-serial NODE1=<log> NODE2=<log>")
		return 2
	}
	var failures []judge.Failure
	for _, n := range []struct {
		name, path   string
		controlPlane bool
		probe        bool
	}{{"node-1", *node1, true, true}, {"node-2", *node2, false, false}} {
		raw, err := os.ReadFile(n.path)
		if err != nil {
			fmt.Fprintln(errOut, "naru-judge:", err)
			return 2
		}
		log := judge.Clean(raw)
		want := judge.Node(n.name, n.controlPlane, n.probe)
		if *gaon {
			interfaces := 1
			if n.controlPlane {
				interfaces = *node1Interfaces
			}
			want = judge.Gaon(n.name, n.controlPlane, interfaces)
			// The issuing is judged when -pki says the Node issued, so a run that issued nothing fails.
			if n.controlPlane && *pki {
				want = append(want, judge.Issuance(judge.GaonIssued...)...)
			}
		}
		bad := judge.Check(n.name, log, want)
		fmt.Fprintf(out, "%s: %s: %d bytes, %d re-printed block(s), %d line(s) dropped by init; %d of %d expectations met\n",
			n.name, n.path, len(raw), log.Reprints, log.Dropped, len(want)-len(bad), len(want))
		failures = append(failures, bad...)
	}
	for _, f := range failures {
		fmt.Fprintln(errOut, "FAIL", f)
	}
	if len(failures) > 0 {
		fmt.Fprintf(errOut, "naru-judge: %d expectation(s) not met\n", len(failures))
		return 1
	}
	fmt.Fprintln(out, "naru-judge: both Nodes show every expectation of the QEMU tests")
	return 0
}
