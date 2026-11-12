// Command naru-judge reads the serial logs of two Nodes and exits 0 only when they show what the
// QEMU tests assert; `make judge-serial NODE1=<log> NODE2=<log>` runs it. The logs are raw
// captures, such as Proxmox's termproxy or `qm terminal` writes. node-1 is the control-plane Node
// and, without -gaon, runs the probe; with -gaon the Nodes run gaon's services in place of the
// stand-ins and the probe, and -pki and -node1-interfaces say what node-1's log must show of its
// issuing and its network, and -naructl names the naructl whose topology lists the services the logs
// must show.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
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
	naructlBin := fs.String("naructl", "", "with -gaon, the naructl binary of the release the Nodes booted from (naructl-<version>-linux-amd64): its topology says which services the logs must show")
	nodeAPI := fs.String("node-api", "", "\"listening\" or \"off\": judge that init serves the Node API on both Nodes, or says it is off (the release's test user-data has no spec.api); left out, the Node API's lines are not judged, so a log of a release before v0.4.0 passes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *nodeAPI != "" && *nodeAPI != "listening" && *nodeAPI != "off" {
		fmt.Fprintln(errOut, "naru-judge: -node-api is listening or off")
		return 2
	}
	if (*pki || *node1Interfaces != 1 || *naructlBin != "") && !*gaon {
		fmt.Fprintln(errOut, "naru-judge: -pki, -node1-interfaces and -naructl are judged only with -gaon")
		return 2
	}
	var top naructl.Topology
	if *gaon {
		if *naructlBin == "" {
			fmt.Fprintln(errOut, "naru-judge: -gaon needs -naructl: the services a Node runs are naructl's to say")
			return 2
		}
		var err error
		if top, err = (naructl.Tool{Bin: *naructlBin}).Topology(); err != nil {
			fmt.Fprintln(errOut, "naru-judge:", err)
			return 2
		}
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
			want = judge.Gaon(top, n.name, n.controlPlane, interfaces)
			// The issuing is judged when -pki says the Node issued, so a run that issued nothing fails.
			if n.controlPlane && *pki {
				want = append(want, judge.Issuance(judge.GaonIssued...)...)
			}
		}
		if *nodeAPI != "" {
			want = append(want, judge.NodeAPI(*nodeAPI == "listening")...)
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
