// Command importcheck fails when a unit imports another unit, when lib/ imports a unit, or when
// the modules under the root differ from go.work's use list (docs/architecture/structure.md).
// `make lint-imports` runs it.
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
	for _, d := range res.Unlisted {
		fmt.Fprintf(os.Stderr, "importcheck: %s holds a go.mod that go.work does not use\n", d)
	}
	for _, d := range res.Missing {
		fmt.Fprintf(os.Stderr, "importcheck: go.work uses %s, which holds no go.mod\n", d)
	}
	for _, v := range res.Violations {
		fmt.Fprintln(os.Stderr, "importcheck:", v)
	}
	if len(res.Violations) > 0 {
		fmt.Fprintf(os.Stderr, "importcheck: %d import(s) cross a unit\n", len(res.Violations))
	}
	if len(res.Violations)+len(res.Unlisted)+len(res.Missing) > 0 {
		os.Exit(1)
	}
}
