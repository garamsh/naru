// Command naru-cpiolist orders and checks a gen_init_cpio list (ADR 0075):
//
//	naru-cpiolist order <list>   writes the list with every directory before its entries to standard output
//	naru-cpiolist check <list>   fails when an entry's parent directory does not come before it
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/garamsh/naru/tooling/disk/internal/cpiolist"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "naru-cpiolist:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 || (args[0] != "order" && args[0] != "check") {
		return fmt.Errorf("usage: naru-cpiolist order|check <list>")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	entries, err := cpiolist.Parse(f)
	if err != nil {
		return fmt.Errorf("%s: %w", args[1], err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s holds no entry", args[1])
	}
	if args[0] == "check" {
		if err := cpiolist.Check(entries); err != nil {
			return fmt.Errorf("%s: %w", args[1], err)
		}
		fmt.Printf("naru-cpiolist: %s: %d entries, every parent directory before its entries\n", args[1], len(entries))
		return nil
	}
	ordered, err := cpiolist.Order(entries)
	if err != nil {
		return fmt.Errorf("%s: %w", args[1], err)
	}
	w := bufio.NewWriter(os.Stdout)
	if err := cpiolist.Format(w, ordered); err != nil {
		return err
	}
	return w.Flush()
}
