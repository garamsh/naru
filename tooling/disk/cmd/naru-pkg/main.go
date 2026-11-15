// Command naru-pkg reads the packages image/pkgs builds, one OCI image layout tar each (ADR 0075):
//
//	naru-pkg unpack <out-dir> <package>.tar...   unpacks each into <out-dir>/<package>
//	naru-pkg digest <package>.tar...             prints "<package> <manifest digest>", one per line
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/tooling/disk/internal/pkg"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "naru-pkg:", err)
		os.Exit(1)
	}
}

func name(file string) string {
	return strings.TrimSuffix(filepath.Base(file), ".tar")
}

func run(args []string) error {
	switch {
	case len(args) >= 3 && args[0] == "unpack":
		for _, f := range args[2:] {
			if err := pkg.Unpack(f, filepath.Join(args[1], name(f))); err != nil {
				return err
			}
		}
		return nil
	case len(args) >= 2 && args[0] == "digest":
		for _, f := range args[1:] {
			d, err := pkg.Digest(f)
			if err != nil {
				return err
			}
			fmt.Println(name(f), d)
		}
		return nil
	}
	return fmt.Errorf("usage: naru-pkg unpack <out-dir> <package>.tar... | naru-pkg digest <package>.tar...")
}
