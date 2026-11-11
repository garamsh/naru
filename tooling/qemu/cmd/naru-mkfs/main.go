// Command naru-mkfs is installed as mkfs.ext4 on a test Node's data disk. It
// runs the mke2fs beside it; see internal/mkfs.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/garamsh/naru/tooling/qemu/internal/mkfs"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "naru-mkfs:", err)
		os.Exit(1)
	}
	argv := mkfs.Argv(filepath.Dir(self), os.Args[1:])
	err = syscall.Exec(argv[0], argv, os.Environ()) //nolint:gosec // argv is built from this binary's own directory
	fmt.Fprintln(os.Stderr, "naru-mkfs: exec:", err)
	os.Exit(1)
}
