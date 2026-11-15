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

// signerCall sends body to the signer and returns the answer's body, or the signer's refusal.
func signerCall(to signerFlags, method, path string, body []byte) ([]byte, error) {
	return signerDo(to, method, path, body, nil)
}

// signerDo is signerCall with request headers, for the endpoint that takes its request on a GET.
// path is the signer's own, /v1/<name>.
func signerDo(to signerFlags, method, path string, body []byte, header map[string]string) ([]byte, error) {
	if *to.config != "" {
		return signerThroughNode(to, method, path, body, header)
	}
	where := "the signer at " + *to.socket
	hc := &http.Client{
		Timeout: callTimeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *to.socket)
		}},
	}
	req, err := http.NewRequest(method, "http://naru-signer"+path, bytes.NewReader(body))
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

// signerThroughNode sends the request through a Node's API, the one client for it.
func signerThroughNode(to signerFlags, method, path string, body []byte, header map[string]string) ([]byte, error) {
	f, err := client.LoadFile(*to.config)
	if err != nil {
		return nil, err
	}
	c, err := client.Dial(f, *to.node)
	if err != nil {
		return nil, err
	}
	where := "the signer through " + c.URL() + client.SignerPath
	ctx, cancel := withTimeout()
	defer cancel()
	answer, err := c.Signer(ctx, method, path, header, body)
	var refused *client.StatusError
	if errors.As(err, &refused) {
		return nil, fmt.Errorf("%s refused (%d %s): %s", where, refused.Status, http.StatusText(refused.Status), refused.Message)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return answer, nil
}
