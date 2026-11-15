package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/hostapi"
)

// bootSetCallTimeout bounds adding a boot set: the upload of a kernel and an initramfs.
const bootSetCallTimeout = 15 * time.Minute

// bootSetManifest is naru-node-<version>.json, the boot set a release carries: the name mvmd
// offers it under, the release's version, and the kernel and the initramfs, each as a file in the
// same directory with its SHA-256 (hex).
type bootSetManifest struct {
	Name      string      `json:"name"`
	Version   string      `json:"version"`
	Kernel    bootSetFile `json:"kernel"`
	Initramfs bootSetFile `json:"initramfs"`
}

// bootSetFile is a file of a boot set.
type bootSetFile struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// readBootSetManifest reads the release's naru-node-<version>.json at path.
func readBootSetManifest(path string) (bootSetManifest, error) {
	var m bootSetManifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s is not a boot set of a release: %w", path, err)
	}
	for _, f := range []bootSetFile{m.Kernel, m.Initramfs} {
		if f.File == "" || f.File != filepath.Base(f.File) || f.SHA256 == "" {
			return m, fmt.Errorf("%s names a file that is not a file name beside it, or no SHA-256 for it", path)
		}
	}
	if m.Name == "" {
		return m, errors.New(path + " names no boot set")
	}
	return m, nil
}

func bootSetAdd(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		m, err := readBootSetManifest(args[0])
		if err != nil {
			return err
		}
		dir := filepath.Dir(args[0])
		kernel, err := os.Open(filepath.Join(dir, m.Kernel.File))
		if err != nil {
			return err
		}
		defer func() { _ = kernel.Close() }()
		initramfs, err := os.Open(filepath.Join(dir, m.Initramfs.File))
		if err != nil {
			return err
		}
		defer func() { _ = initramfs.Close() }()
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), bootSetCallTimeout)
		defer cancel()
		if err := cl.AddBootSet(ctx, hostapi.BootSetRequest{Name: m.Name, KernelSHA256: m.Kernel.SHA256, InitramfsSHA256: m.Initramfs.SHA256}, kernel, initramfs); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "added boot set %s (%s)\n", m.Name, m.Version)
		return nil
	}
}

func bootSetRemove(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.RemoveBootSet(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "removed boot set %s\n", args[0])
		return nil
	}
}
