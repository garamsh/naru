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

	"github.com/garamsh/naru/naructl/internal/client"
)

// bootSetCallTimeout bounds adding a boot set: the upload of a kernel and an initramfs.
const bootSetCallTimeout = 15 * time.Minute

// bootSet runs the subcommands of naructl host boot-set.
func bootSet(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("host boot-set needs add or remove")
	}
	switch args[0] {
	case "add":
		return bootSetAdd(args[1:], stdout, stderr)
	case "remove":
		return bootSetRemove(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "host boot-set "+args[0])
}

// readBootSetManifest reads the release's naru-node-<version>.json at path.
func readBootSetManifest(path string) (client.BootSetManifest, error) {
	var m client.BootSetManifest
	b, err := os.ReadFile(path) //nolint:gosec // an argument of the command
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s is not a boot set of a release: %w", path, err)
	}
	for _, f := range []client.BootSetFile{m.Kernel, m.Initramfs} {
		if f.File == "" || f.File != filepath.Base(f.File) || f.SHA256 == "" {
			return m, fmt.Errorf("%s names a file that is not a file name beside it, or no SHA-256 for it", path)
		}
	}
	if m.Name == "" {
		return m, errors.New(path + " names no boot set")
	}
	return m, nil
}

func bootSetAdd(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host boot-set add", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	m, err := readBootSetManifest(rest[0])
	if err != nil {
		return err
	}
	dir := filepath.Dir(rest[0])
	kernel, err := os.Open(filepath.Join(dir, m.Kernel.File)) //nolint:gosec // a name beside the manifest
	if err != nil {
		return err
	}
	defer func() { _ = kernel.Close() }()
	initramfs, err := os.Open(filepath.Join(dir, m.Initramfs.File)) //nolint:gosec // a name beside the manifest
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
	if err := cl.AddBootSet(ctx, m, kernel, initramfs); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "added boot set %s (%s)\n", m.Name, m.Version)
	return nil
}

func bootSetRemove(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host boot-set remove", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	if err := cl.RemoveBootSet(ctx, rest[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "removed boot set %s\n", rest[0])
	return nil
}
