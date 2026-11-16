package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
)

// extensionTimeout bounds an extension upload: an image is tens of megabytes.
const extensionTimeout = 10 * time.Minute

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// extensionAdd hashes the image, then streams it with the digest, so a Node that sees other bytes
// refuses them.
func extensionAdd(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h := sha256.New()
		size, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		digest := hex.EncodeToString(h.Sum(nil))
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), extensionTimeout)
		defer cancel()
		e, err := cl.PutExtension(ctx, digest, f, size)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "the Node stores %s (%s) built for naru %s: sha256 %s\n", e.Name, strings.Join(e.Services, ","), e.Version, e.SHA256)
		_, _ = fmt.Fprintf(stdout, "spec.extensions names it as {name: %s, sha256: %s}, and takes effect at the next boot\n", e.Name, e.SHA256)
		return nil
	}
}

func extensionList(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		list, err := cl.Extensions(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tVERSION\tSHA256\tSIZE\tSERVICES\tMOUNTED")
		for _, e := range list {
			mounted := "no"
			if e.Mounted {
				mounted = e.MountPoint
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", e.Name, e.Version, e.SHA256, e.Size, strings.Join(e.Services, ","), mounted)
		}
		return w.Flush()
	}
}

func extensionRemove(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		if !sha256RE.MatchString(args[0]) {
			return fmt.Errorf("%q is not a lowercase hex SHA-256", args[0])
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteExtension(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "the Node removed %s\n", args[0])
		return nil
	}
}
