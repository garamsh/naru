// Command modcheck fails when two modules of go.work require one dependency at different versions,
// or when a go.mod's go directive is not go.work's (docs/architecture/structure.md). `make lint-modules`
// runs it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/garamsh/naru/tooling/importcheck/internal/modcheck"
)

func main() {
	root := flag.String("root", ".", "the repository root")
	flag.Parse()
	res, err := modcheck.Check(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modcheck:", err)
		os.Exit(1)
	}
	fmt.Printf("modcheck: compared %d dependenc(ies) required by %d module(s) of %s/go.work\n", res.Dependencies, res.Modules, *root)
	for _, s := range res.Skews {
		fmt.Fprintln(os.Stderr, "modcheck:", s)
	}
	for _, d := range res.WrongGo {
		fmt.Fprintf(os.Stderr, "modcheck: %s/go.mod has a go directive other than go.work's\n", d)
	}
	if len(res.Skews)+len(res.WrongGo) > 0 {
		os.Exit(1)
	}
}
