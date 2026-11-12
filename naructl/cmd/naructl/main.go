// Command naructl is the operator's tool: it generates a cluster's certificate authority, each
// Node's user-data and an operator's client certificate.
package main

import (
	"fmt"
	"os"

	"github.com/garamsh/naru/naructl/internal/command"
)

func main() {
	if err := command.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "naructl:", err)
		os.Exit(1)
	}
}
