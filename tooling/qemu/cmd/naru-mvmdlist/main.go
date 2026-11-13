// Command naru-mvmdlist is the service the QEMU test of the host API configures (ADR 0028). It asks
// mvmd's runtime API for its Machines, as mvmd's own ListMachines answers, and prints on the console
// the Machine IDs and their states whenever the list changes. The test reads those lines to see what
// mvmd holds, which naru-hostd's own answers cannot show.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func main() {
	socket := flag.String("socket", "/run/mvmd.sock", "mvmd's runtime API socket")
	interval := flag.Duration("interval", time.Second, "the time between two lists")
	flag.Parse()
	last := "\x00"
	for ; ; time.Sleep(*interval) {
		line, err := list(*socket)
		if err != nil {
			line = "unreachable"
		}
		if line != last {
			fmt.Println("naru88-machines:", line)
			last = line
		}
	}
}

// list returns "none", or the Machines as id=STATE joined by commas, in order.
func list(socket string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := runtimeapi.Dial(ctx, socket)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	r, err := v1alpha2.NewRuntimeServiceClient(conn).ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	if err != nil {
		return "", err
	}
	var ms []string
	for _, m := range r.GetMachines() {
		ms = append(ms, m.GetId()+"="+strings.TrimPrefix(m.GetState().String(), "MACHINE_STATE_"))
	}
	if len(ms) == 0 {
		return "none", nil
	}
	sort.Strings(ms)
	return strings.Join(ms, ","), nil
}
