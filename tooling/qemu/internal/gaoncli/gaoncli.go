// Package gaoncli runs gaon's command line client, built from the commit image/gaon/pin.env names,
// against a test cluster's API server, with the test host's client certificate. The client is a
// test input on the host; the image does not carry it.
package gaoncli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// Client is gaon's CLI with the flags that reach one cluster.
type Client struct {
	bin  string
	flag []string
}

// New returns a Client that runs bin against the API server at addr, "127.0.0.1:port", with the
// CA and the client pair naructl wrote.
func New(bin, addr string, client naructl.Client) *Client {
	return &Client{bin: bin, flag: []string{
		"--server", "https://" + addr + hostapi.Root,
		"--tls-ca", client.CA,
		"--tls-cert", client.Cert,
		"--tls-key", client.Key,
	}}
}

// Run runs the CLI with args after its global flags and returns its combined output. A non-zero
// exit is an error that holds the output.
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, c.bin, append(append([]string{}, c.flag...), args...)...).CombinedOutput() //nolint:gosec // the CLI the build wrote
	if err != nil {
		return string(out), fmt.Errorf("gaon %v: %w: %s", args, err, out)
	}
	return string(out), nil
}

const (
	// conflictTries is how many times Apply runs the CLI while the object keeps changing under it.
	conflictTries = 10
	// conflictDelay is the wait between two of them.
	conflictDelay = time.Second
)

// Apply writes manifest to a file in dir and runs `gaon apply -f` on it. The CLI reads the object and
// writes it back with the version it read, so it refuses with a Conflict when a controller or an
// agent wrote the object in between, as the controllers do right after a Node turns Ready. That is
// the CLI telling its operator to run it again, so Apply does, up to conflictTries times.
func (c *Client) Apply(ctx context.Context, dir, manifest string) (string, error) {
	p := filepath.Join(dir, "apply.yaml")
	if err := os.WriteFile(p, []byte(manifest), 0o600); err != nil {
		return "", err
	}
	for try := 1; ; try++ {
		out, err := c.Run(ctx, "apply", "-f", p)
		if err == nil || try == conflictTries || !strings.Contains(out, "Conflict:") {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(conflictDelay):
		}
	}
}
