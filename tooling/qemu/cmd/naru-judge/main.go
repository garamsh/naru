// Command naru-judge reads the serial logs of two Nodes and exits 0 only when they show what the
// QEMU tests assert; `make judge-serial NODE1=<log> NODE2=<log>` runs it. The logs are raw
// captures, such as Proxmox's termproxy or `qm terminal` writes. Without -release they are the logs
// of the stand-in Nodes `make nodes` boots, and node-1 runs the probe; with -release they are the
// logs of Nodes booted from the release's naru-node-<n>.test.yaml, which run mvmd and the registry.
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
	node1 := fs.String("node1", "", "serial log of node-1, which runs the probe without -release")
	node2 := fs.String("node2", "", "serial log of node-2")
	released := fs.Bool("release", false, "the Nodes booted from the release's test user-data, not the stand-ins and the probe")
	nodeAPI := fs.String("node-api", "", "\"listening\" or \"off\": judge that init serves the Node API on both Nodes, or says it is off (the release's test user-data has no spec.api); left out, the Node API's lines are not judged, so a log of a release before v0.4.0 passes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *nodeAPI != "" && *nodeAPI != "listening" && *nodeAPI != "off" {
		fmt.Fprintln(errOut, "naru-judge: -node-api is listening or off")
		return 2
	}
	if *node1 == "" || *node2 == "" {
		fmt.Fprintln(errOut, "naru-judge: -node1 and -node2 are required; make judge-serial NODE1=<log> NODE2=<log>")
		return 2
	}
	var failures []judge.Failure
	for _, n := range []struct {
		name, path string
		probe      bool
	}{{"node-1", *node1, true}, {"node-2", *node2, false}} {
		raw, err := os.ReadFile(n.path)
		if err != nil {
			fmt.Fprintln(errOut, "naru-judge:", err)
			return 2
		}
		log := judge.Clean(raw)
		want := judge.Node(n.name, n.probe)
		if *released {
			want = judge.Released(n.name)
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
