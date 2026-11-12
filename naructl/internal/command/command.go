// Package command is naructl's command line: the gen commands, topology, and the Node API's client commands.
package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

const usage = `usage: naructl gen <ca|client|token|node|api-ca|api-client> [flags]
       naructl <version|services|logs|get config|apply|reboot> --config <client file> [--node <ip[:port]>] [flags]
       naructl topology                              print the services a Node runs, and which signs, as JSON

  gen api-ca  --ca-dir <dir>                    write the Node API authority, <dir>/api/ca.pem and ca.key
  gen api-client --ca-dir <dir> --role admin|reader --endpoint <ip[:port]> --out <file>
              write a client file for the Node API
  version | services | get config               read a Node
  logs <service> [-f]                           print a service's kept log, and with -f each new line
  apply -f <patch> [--mode no-reboot|staged|reboot] [--dry-run]
              merge a patch into the Node's configuration
  reboot      stop the Node's services and reboot it
  gen ca      --out <dir>                       write ca.pem and ca.key
  gen client  --ca <dir> --cn <name> --out <dir> write an operator's client.pem and client.key
  gen token   --ca <dir> --name <name>              write the Node's bootstrap token to <dir>/tokens/<name>, if none is there
  gen node    --ca <dir> --name <name> --address <cidr> --mac <mac> --gateway <ip> --nameserver <ip>
              --api-server <ip> --machine-prefix <cidr> --guest <guest.json> --firecracker-sha256 <hex>
              --out <file> [--control-plane] [--peer <address>=<machine-prefix>] [--time-server <host[:port]>] [more flags: naructl gen node -h]
`

// Run runs naructl with args, the command line without the program's name.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && args[0] == "topology" {
		b, err := layout.Gaon.JSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "version":
			return version(args[1:], stdout, stderr)
		case "services":
			return services(args[1:], stdout, stderr)
		case "logs":
			return logs(args[1:], stdout, stderr)
		case "apply":
			return apply(args[1:], stdout, stderr)
		case "reboot":
			return reboot(args[1:], stdout, stderr)
		case "get":
			if len(args) > 1 && args[1] == "config" {
				return getConfig(args[2:], stdout, stderr)
			}
		}
	}
	if len(args) < 2 || args[0] != "gen" {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[1] {
	case "api-ca":
		return genAPICA(args[2:], stdout, stderr)
	case "api-client":
		return genAPIClient(args[2:], stdout, stderr)
	case "ca":
		return genCA(args[2:], stdout, stderr)
	case "client":
		return genClient(args[2:], stdout, stderr)
	case "token":
		return genToken(args[2:], stdout, stderr)
	case "node":
		return genNode(args[2:], stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", "gen "+args[1])
}

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("naructl "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func require(fs *flag.FlagSet, names ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}

// needs refuses each of the flags given without the flag that makes it apply, which would otherwise
// be accepted and do nothing.
func needs(fs *flag.FlagSet, with string, flags ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set[with] {
		return nil
	}
	for _, f := range flags {
		if set[f] {
			return fmt.Errorf("--%s has no effect without --%s", f, with)
		}
	}
	return nil
}

func genCA(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen ca", stderr)
	out := fs.String("out", "", "directory that receives ca.pem and ca.key (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "out"); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*out, pki.KeyFile)); err == nil {
		return fmt.Errorf("%s already holds an authority; a new one would replace its key", *out)
	}
	ca, err := pki.New("naru-ca")
	if err != nil {
		return err
	}
	if err := ca.Write(*out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s; keep %s in your secret store\n", filepath.Join(*out, pki.CertFile), filepath.Join(*out, pki.KeyFile), pki.KeyFile)
	return nil
}

func genClient(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen client", stderr)
	caDir := fs.String("ca", "", "directory of ca.pem and ca.key")
	cn := fs.String("cn", "", "the certificate's CN, the caller's identity to gaon")
	out := fs.String("out", "", "directory that receives client.pem and client.key (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "cn", "out"); err != nil {
		return err
	}
	ca, err := pki.Load(*caDir)
	if err != nil {
		return err
	}
	pair, err := ca.Client(*cn)
	if err != nil {
		return err
	}
	if err := pki.WritePair(*out, "client", pair); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s for CN %s\n", filepath.Join(*out, "client.pem"), filepath.Join(*out, "client.key"), *cn)
	return nil
}

func genToken(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen token", stderr)
	caDir := fs.String("ca", "", "directory of the cluster's authority, which holds tokens/")
	name := fs.String("name", "", "the Node's name, such as node-2")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name"); err != nil {
		return err
	}
	if _, err := pki.Load(*caDir); err != nil {
		return err
	}
	if _, err := pki.EnsureToken(*caDir, *name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: %s holds the token of %s\n", filepath.Join(*caDir, pki.TokensDir, *name), *name)
	return nil
}

func genNode(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen node", stderr)
	var (
		nameservers, serving, binaries, disable, timeServers, peers, members list
		n                                                                    layout.Node
		caDir, address, gateway, apiServer                                   string
		machinePrefix, guest, extraNIC                                       string
		out, networkConfig                                                   string
		controlPlane, pairsAsFiles                                           bool
	)
	fs.StringVar(&caDir, "ca", "", "directory of ca.pem and ca.key (the key is read here and never carried by a worker)")
	fs.StringVar(&n.Name, "name", "", "the Node's name, such as node-1")
	fs.BoolVar(&controlPlane, "control-plane", false, "the Node runs etcd and gaon's control plane, issues its certificates at boot and signs its agents' requests")
	fs.StringVar(&address, "address", "", "the Node's address and prefix, such as 192.168.0.40/24")
	fs.StringVar(&n.MAC, "mac", "", "the MAC address of the Node's card (with --network-config)")
	fs.StringVar(&gateway, "gateway", "", "the default gateway (with --network-config)")
	fs.Var(&nameservers, "nameserver", "a nameserver, repeatable (with --network-config)")
	fs.StringVar(&n.Search, "search", "", "the search domain (with --network-config)")
	fs.StringVar(&extraNIC, "extra-nic", "", "a second card, <mac>=<cidr>, with no gateway (with --network-config)")
	fs.StringVar(&apiServer, "api-server", "", "the address every agent and naructl dial: the control-plane Node's, or the virtual address of several (--member)")
	fs.Var(&members, "member", "<name>=<address>, a control-plane Node of a control plane of several, repeatable, in priority order and the same on every member, this Node among them (with --control-plane)")
	fs.Var(&serving, "serving-address", "another address the API server's certificate and the Node API's certificate name, repeatable")
	fs.Var(&binaries, "binary", "<service>=<absolute path>, the executable of a service in place of the image's, repeatable")
	fs.Var(&disable, "disable", "a service to leave out ("+strings.Join(layout.Gaon.Names(), ", ")+"), repeatable")
	fs.Var(&timeServers, "time-server", "an SNTP server init sets the clock from, host or host:port, repeatable and asked in order (none: the clock is not set)")
	fs.StringVar(&machinePrefix, "machine-prefix", "", "the IPv4 prefix mvmd gives its Machines, such as 10.200.1.0/24")
	fs.Var(&peers, "peer", "another Node of the cluster, <address>=<machine-prefix> such as 192.168.0.61=10.200.2.0/24, repeatable: the Node routes to the prefix via the address, and its Machines keep their source address to it")
	fs.StringVar(&guest, "guest", "", "naru-guest-<version>.json, the guest manifest")
	fs.StringVar(&n.FirecrackerSHA256, "firecracker-sha256", "", "the SHA-256 of the Firecracker binary the image carries")
	fs.BoolVar(&pairsAsFiles, "pairs-as-files", false, "the Node carries operator-issued pairs as files, with no bootstrap token: on a control-plane Node every pair, and no signing key")
	fs.StringVar(&out, "out", "", "the user-data file to write (0600: it carries keys; a file already there is replaced)")
	fs.StringVar(&networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name", "address", "api-server", "machine-prefix", "guest", "firecracker-sha256", "out"); err != nil {
		return err
	}
	if err := needs(fs, "network-config", "mac", "gateway", "nameserver", "search", "extra-nic"); err != nil {
		return err
	}
	if err := needs(fs, "control-plane", "serving-address", "member"); err != nil {
		return err
	}
	var err error
	if n.CA, err = pki.Load(caDir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(caDir, pki.APIDir)); err == nil {
		if n.API, err = pki.Load(filepath.Join(caDir, pki.APIDir)); err != nil {
			return fmt.Errorf("the Node API authority: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	n.Mode = layout.ModeOf(controlPlane, pairsAsFiles)
	// The Node's own token is made if there is none. A control-plane Node's API server lists the
	// token of every Node whose file is in the directory now: run gen token for the others first.
	if n.Token, n.Tokens, err = n.Mode.LoadTokens(caDir, n.Name); err != nil {
		return err
	}
	if n.Address, err = netip.ParsePrefix(address); err != nil {
		return fmt.Errorf("--address: %w", err)
	}
	if n.APIServer, err = netip.ParseAddr(apiServer); err != nil {
		return fmt.Errorf("--api-server: %w", err)
	}
	if n.MachinePrefix, err = netip.ParsePrefix(machinePrefix); err != nil {
		return fmt.Errorf("--machine-prefix: %w", err)
	}
	if gateway != "" {
		if n.Gateway, err = netip.ParseAddr(gateway); err != nil {
			return fmt.Errorf("--gateway: %w", err)
		}
	}
	for _, s := range nameservers {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--nameserver: %w", err)
		}
		n.Nameservers = append(n.Nameservers, a)
	}
	for _, s := range serving {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--serving-address: %w", err)
		}
		n.ServingAddresses = append(n.ServingAddresses, a)
	}
	for _, s := range peers {
		addr, prefix, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--peer %q is not <address>=<machine-prefix>", s)
		}
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		n.Peers = append(n.Peers, layout.Peer{Address: a, MachinePrefix: p})
	}
	for _, m := range members {
		name, addr, ok := strings.Cut(m, "=")
		a, err := netip.ParseAddr(addr)
		if !ok || err != nil {
			return fmt.Errorf("--member %q is not <name>=<address>", m)
		}
		n.Members = append(n.Members, layout.Member{Name: name, Address: a})
	}
	if extraNIC != "" {
		mac, cidr, ok := strings.Cut(extraNIC, "=")
		if !ok {
			return fmt.Errorf("--extra-nic %q is not <mac>=<cidr>", extraNIC)
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("--extra-nic: %w", err)
		}
		n.ExtraNIC = &layout.NIC{MAC: mac, Address: p}
	}
	n.Binaries = map[string]string{}
	for _, b := range binaries {
		name, bin, ok := strings.Cut(b, "=")
		if !ok {
			return fmt.Errorf("--binary %q is not <service>=<path>", b)
		}
		n.Binaries[name] = bin
	}
	n.Disable = disable
	n.TimeServers = timeServers
	if n.Guest, err = layout.ReadManifest(guest); err != nil {
		return err
	}
	userData, err := n.UserData()
	if err != nil {
		return err
	}
	var nc []byte
	if networkConfig != "" {
		if nc, err = n.NetworkConfig(); err != nil {
			return err
		}
	}
	if err := pki.WriteFile(out, userData, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s", out)
	if nc != nil {
		if err := os.WriteFile(networkConfig, nc, 0o644); err != nil { //nolint:gosec // carries no key
			return err
		}
		_, _ = fmt.Fprintf(stdout, " and %s", networkConfig)
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}
