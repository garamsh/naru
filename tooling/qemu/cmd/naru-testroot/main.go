// Command naru-testroot writes the throwaway root that make test-qemu appends to the test image's
// bundle (TEST_ROOT): ca.pem and ca.key in the directory -out.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

func main() {
	out := flag.String("out", "", "directory to write ca.pem and ca.key into")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "naru-testroot: -out is required")
		os.Exit(2)
	}
	if err := fixture.WriteRoot(*out); err != nil {
		fmt.Fprintln(os.Stderr, "naru-testroot:", err)
		os.Exit(1)
	}
}
