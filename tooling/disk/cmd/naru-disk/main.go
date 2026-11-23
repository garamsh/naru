// Command naru-disk builds naru's UEFI disk image from the kernel, which carries its initramfs.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/garamsh/naru/tooling/disk/internal/disk"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "naru-disk:", err)
		os.Exit(1)
	}
}

func run() error {
	var s disk.Spec
	flag.StringVar(&s.Kernel, "kernel", "", "the kernel with the initramfs embedded, bzImage")
	flag.StringVar(&s.Out, "out", "", "the disk image to write")
	flag.StringVar(&s.DiskGUID, "disk-guid", "", "the GUID of the disk")
	flag.StringVar(&s.ESPGUID, "esp-guid", "", "the GUID of the EFI system partition")
	flag.Parse()
	if s.Kernel == "" || s.Out == "" || s.DiskGUID == "" || s.ESPGUID == "" {
		flag.Usage()
		return errors.New("-kernel, -out, -disk-guid and -esp-guid are required")
	}
	epoch, err := strconv.ParseInt(os.Getenv("SOURCE_DATE_EPOCH"), 10, 64)
	if err != nil {
		return fmt.Errorf("SOURCE_DATE_EPOCH must be set to a Unix time (image/build.env): %w", err)
	}
	s.Stamp = time.Unix(epoch, 0)
	if err := disk.Build(s); err != nil {
		return err
	}
	fmt.Printf("naru-disk: wrote %s\n", s.Out)
	return nil
}
