package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/hostapi"
)

func hostNetworkPut(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	var members list
	prefix := c.fs.String("prefix", "", "the prefix of the network's routed space")
	endpoint := c.fs.String("api-endpoint", "", "the address of the network's API endpoint, inside the prefix")
	c.fs.Var(&members, "remote-member", "an address of a microVM of the network on another host, in CIDR form (/32), repeatable; the list replaces the host's")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
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
		n, err := cl.PutNetwork(ctx, args[0], hostapi.NetworkRequest{Prefix: *prefix, APIEndpoint: *endpoint, RemoteMembers: members})
		if err != nil {
			return err
		}
		return printJSON(stdout, n)
	}
}

func hostNetworkGet(fs *pflag.FlagSet) runner {
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
		n, err := cl.Network(ctx, args[0])
		if err != nil {
			return err
		}
		return printJSON(stdout, n)
	}
}

func hostNetworkList(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
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
		all, err := cl.Networks(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, all)
	}
}

func hostNetworkDelete(fs *pflag.FlagSet) runner {
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
		if err := cl.DeleteNetwork(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "deleted network %s\n", args[0])
		return nil
	}
}

func hostPeersGet(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
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
		peers, err := cl.Peers(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, peers)
	}
}

func hostPeersSet(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	file := c.fs.StringP("file", "f", "", "the whole list of peers as a JSON file, {\"meshAddress\": ..., \"peers\": [...]}, which replaces the host's")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
			return err
		}
		if err := require(c.fs, "file"); err != nil {
			return err
		}
		b, err := os.ReadFile(*file)
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
		peers, err := cl.SetPeers(ctx, req)
		if err != nil {
			return err
		}
		return printJSON(stdout, peers)
	}
}

// hostTunnel is naructl host tunnel: what another host needs to peer with this one.
func hostTunnel(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
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
		t, err := cl.Tunnel(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, t)
	}
}
