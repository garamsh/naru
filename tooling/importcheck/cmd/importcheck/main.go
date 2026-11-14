// Command importcheck fails when a unit imports another unit, or lib/ imports a unit
// (docs/architecture/structure.md). `make lint-imports` runs it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/garamsh/naru/tooling/importcheck/internal/importcheck"
)

func main() {
	root := flag.String("root", ".", "the repository root")
	flag.Parse()
	res, err := importcheck.Check(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "importcheck:", err)
		os.Exit(1)
	}
	fmt.Printf("importcheck: read %d Go file(s) of %d module(s) found by go.mod under %s\n", res.Files, res.Modules, *root)
	if len(res.Violations) == 0 {
		return
	}
	for _, v := range res.Violations {
		fmt.Fprintln(os.Stderr, "importcheck:", v)
	}
	fmt.Fprintf(os.Stderr, "importcheck: %d import(s) cross a unit\n", len(res.Violations))
	os.Exit(1)
}
