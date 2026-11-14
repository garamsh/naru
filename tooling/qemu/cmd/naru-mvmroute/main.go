// Command naru-mvmroute is the service of a QEMU Node that gives the test host a route to the
// addresses of the microVMs on the Node's Machine prefix (docs/architecture/harness.md). It listens on
// the address of the Node's second NIC and dials only inside the prefix it is given.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/garamsh/naru/tooling/qemu/internal/route"
)

func main() {
	listen := flag.String("listen", "10.0.2.15:7480", "the address of the Node's second NIC and the route's port")
	prefix := flag.String("prefix", "", "the Node's Machine prefix, the only addresses the route dials")
	flag.Parse()
	p, err := netip.ParsePrefix(*prefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "naru-mvmroute: --prefix:", err)
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "naru-mvmroute:", err)
		os.Exit(1)
	}
	fmt.Println("naru-mvmroute: routing to", p, "on", *listen)
	if err := route.Serve(ln, route.WithinPrefix(p)); err != nil {
		fmt.Fprintln(os.Stderr, "naru-mvmroute:", err)
		os.Exit(1)
	}
}
