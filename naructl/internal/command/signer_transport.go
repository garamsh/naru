package command

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/garamsh/naru/naructl/internal/client"
)

// signerFlags are the flags of a command that calls the signer: --socket reaches it on this
// host, and --config with --node reaches it through a Node's API, whose admin role forwards
// /v1/signer to it.
type signerFlags struct {
	socket, config, node *string
}

func addSignerFlags(fs *flag.FlagSet) signerFlags {
	return signerFlags{
		socket: fs.String("socket", "", "naru-signer's unix socket, for a command run on its host"),
		config: fs.String("config", "", "the Node API client file gen api-client wrote, an admin's, to reach the signer through a Node's API"),
		node:   fs.String("node", "", "with --config, the Node that runs the signer: an IP address with or without a port (default the client file's first endpoint)"),
	}
}

// check refuses a command given both --socket and --config or neither, and --node without --config.
func (s signerFlags) check(fs *flag.FlagSet) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case set["socket"] && set["config"]:
		return errors.New("--socket and --config are alternatives: give one")
	case !set["socket"] && !set["config"]:
		return errors.New("--socket or --config is required")
	}
	return needs(fs, "config", "node")
}

// transport returns the HTTP client that reaches the signer, the URL its /v1 paths are served
// under, and where that is for an error message.
func (s signerFlags) transport() (hc *http.Client, base, where string, err error) {
	if *s.config == "" {
		return &http.Client{
			Timeout: callTimeout,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", *s.socket)
			}},
		}, "http://naru-signer/v1", "the signer at " + *s.socket, nil
	}
	f, err := client.LoadFile(*s.config)
	if err != nil {
		return nil, "", "", err
	}
	c, err := client.Dial(f, *s.node)
	if err != nil {
		return nil, "", "", err
	}
	shared, base := c.Signer()
	own := *shared
	own.Timeout = callTimeout
	return &own, base, "the signer through " + base, nil
}

// signerCall sends body to the signer and returns the answer's body, or the signer's refusal.
func signerCall(to signerFlags, method, path string, body []byte) ([]byte, error) {
	return signerDo(to, method, path, body, nil)
}

// signerDo is signerCall with request headers, for the endpoint that takes its request on a GET.
// path is the signer's own, /v1/<name>.
func signerDo(to signerFlags, method, path string, body []byte, header map[string]string) ([]byte, error) {
	hc, base, where, err := to.transport()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, base+strings.TrimPrefix(path, "/v1"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s refused (%s): %s", where, resp.Status, strings.TrimSpace(string(answer)))
	}
	return answer, nil
}
