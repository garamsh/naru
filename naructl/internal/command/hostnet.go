package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/garamsh/naru/lib/hostapi"
)

// hostNetwork runs the subcommands of naructl host network: the networks a host declares, which
// are the host API's and not the management plane's (naructl network).
func hostNetwork(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("host network needs put, get, list or delete")
	}
	switch args[0] {
	case "put":
		return hostNetworkPut(args[1:], stdout, stderr)
	case "get":
		return hostNetworkGet(args[1:], stdout, stderr)
	case "list":
		return hostNetworkList(args[1:], stdout, stderr)
	case "delete":
		return hostNetworkDelete(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "host network "+args[0])
}

func hostNetworkPut(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host network put", stderr)
	var members list
	prefix := c.fs.String("prefix", "", "the prefix of the network's Pod and routed space")
	endpoint := c.fs.String("api-endpoint", "", "the address of the network's API endpoint, inside the prefix")
	c.fs.Var(&members, "remote-member", "an address or pod prefix of a microVM of the network on another host, in CIDR form, repeatable; the list replaces the host's")
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	if err := require(c.fs, "prefix"); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	n, err := cl.PutNetwork(ctx, rest[0], hostapi.NetworkRequest{Prefix: *prefix, APIEndpoint: *endpoint, RemoteMembers: members})
	if err != nil {
		return err
	}
	return printJSON(stdout, n)
}

func hostNetworkGet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host network get", stderr)
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
	n, err := cl.Network(ctx, rest[0])
	if err != nil {
		return err
	}
	return printJSON(stdout, n)
}

func hostNetworkList(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host network list", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	all, err := cl.Networks(ctx)
	if err != nil {
		return err
	}
	return printJSON(stdout, all)
}

func hostNetworkDelete(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host network delete", stderr)
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
	if err := cl.DeleteNetwork(ctx, rest[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "deleted network %s\n", rest[0])
	return nil
}

// hostPeers runs the subcommands of naructl host peers: the hosts this host's tunnel reaches.
func hostPeers(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("host peers needs get or set")
	}
	switch args[0] {
	case "get":
		return hostPeersGet(args[1:], stdout, stderr)
	case "set":
		return hostPeersSet(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "host peers "+args[0])
}

func hostPeersGet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host peers get", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	peers, err := cl.Peers(ctx)
	if err != nil {
		return err
	}
	return printJSON(stdout, hostapi.PeersRequest{Peers: peers})
}

func hostPeersSet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host peers set", stderr)
	file := c.fs.String("f", "", "the whole list of peers as a JSON file, {\"peers\": [...]}, which replaces the host's")
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	if err := require(c.fs, "f"); err != nil {
		return err
	}
	b, err := os.ReadFile(*file) //nolint:gosec // an argument of the command
	if err != nil {
		return err
	}
	var req hostapi.PeersRequest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errors.New(*file + " is not a list of peers: " + err.Error())
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	peers, err := cl.SetPeers(ctx, req.Peers)
	if err != nil {
		return err
	}
	return printJSON(stdout, hostapi.PeersRequest{Peers: peers})
}

// hostTunnel is naructl host tunnel: what another host needs to peer with this one.
func hostTunnel(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host tunnel", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	t, err := cl.Tunnel(ctx)
	if err != nil {
		return err
	}
	return printJSON(stdout, t)
}
