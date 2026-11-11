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

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

// Client is gaon's CLI with the flags that reach one cluster.
type Client struct {
	bin  string
	flag []string
}

// New returns a Client that runs bin against the API server at addr, "127.0.0.1:port", with the
// cluster's CA and the client pair certs holds for the test host. It writes those files into dir,
// the keys 0600.
func New(bin, addr string, certs *pki.Cluster, dir string) (*Client, error) {
	files := map[string][]byte{"ca.pem": certs.CA, "client.pem": certs.Client.CertPEM, "client.key": certs.Client.KeyPEM}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	return &Client{bin: bin, flag: []string{
		"--server", "https://" + addr + hostapi.Root,
		"--tls-ca", filepath.Join(dir, "ca.pem"),
		"--tls-cert", filepath.Join(dir, "client.pem"),
		"--tls-key", filepath.Join(dir, "client.key"),
	}}, nil
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

// Apply writes manifest to a file in dir and runs `gaon apply -f` on it.
func (c *Client) Apply(ctx context.Context, dir, manifest string) (string, error) {
	p := filepath.Join(dir, "apply.yaml")
	if err := os.WriteFile(p, []byte(manifest), 0o600); err != nil {
		return "", err
	}
	return c.Run(ctx, "apply", "-f", p)
}
