// Command naru-disk builds naru's UEFI disk image from the kernel, which carries its initramfs.
package main

import (
	"encoding/json"
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
	var layout string
	flag.StringVar(&layout, "layout", "", "a file to write what the ESP spends on the extensions to, as JSON, for the release")
	var exts stringList
	flag.Var(&exts, "extension", "the description (<name>.json) of a system extension the ESP carries, beside its <name>.sqfs; repeatable")
	flag.Parse()
	for _, e := range exts {
		s.Extensions = append(s.Extensions, disk.Extension{Description: e})
	}
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
	if layout != "" {
		l, err := s.Layout()
		if err != nil {
			return err
		}
		raw, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if err := os.WriteFile(layout, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("naru-disk: wrote %s\n", s.Out)
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return fmt.Sprint(*l) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
