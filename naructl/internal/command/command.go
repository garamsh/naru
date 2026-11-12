// Package command is naructl's command line: gen ca, gen client and gen node.
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

const usage = `usage: naructl gen <ca|client|node> [flags]

  gen ca      --out <dir>                       write ca.pem and ca.key
  gen client  --ca <dir> --cn <name> --out <dir> write an operator's client.pem and client.key
  gen node    --ca <dir> --name <name> --address <cidr> --mac <mac> --gateway <ip> --nameserver <ip>
              --api-server <ip> --machine-prefix <cidr> --guest <guest.json> --firecracker-sha256 <hex>
              --out <file> [--control-plane] [more flags: naructl gen node -h]
`

// Run runs naructl with args, the command line without the program's name.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "gen" {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[1] {
	case "ca":
		return genCA(args[2:], stdout, stderr)
	case "client":
		return genClient(args[2:], stdout, stderr)
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
	fs := flag.NewFlagSet("naructl gen "+name, flag.ContinueOnError)
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

func genCA(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("ca", stderr)
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
	fs := newFlags("client", stderr)
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

func genNode(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("node", stderr)
	var (
		nameservers, serving, binaries, disable list
		n                                       layout.Node
		caDir, address, gateway, apiServer      string
		machinePrefix, guest, extraNIC          string
		out, networkConfig                      string
	)
	fs.StringVar(&caDir, "ca", "", "directory of ca.pem and ca.key (the key is read here and never carried by a worker)")
	fs.StringVar(&n.Name, "name", "", "the Node's name, such as node-1")
	fs.BoolVar(&n.ControlPlane, "control-plane", false, "the Node runs etcd and gaon's control plane, and issues its certificates at boot")
	fs.StringVar(&address, "address", "", "the Node's address and prefix, such as 192.168.0.40/24")
	fs.StringVar(&n.MAC, "mac", "", "the MAC address of the Node's card (with --network-config)")
	fs.StringVar(&gateway, "gateway", "", "the default gateway (with --network-config)")
	fs.Var(&nameservers, "nameserver", "a nameserver, repeatable (with --network-config)")
	fs.StringVar(&n.Search, "search", "", "the search domain (with --network-config)")
	fs.StringVar(&extraNIC, "extra-nic", "", "a second card, <mac>=<cidr>, with no gateway (with --network-config)")
	fs.StringVar(&apiServer, "api-server", "", "the control-plane Node's address, which every agent dials")
	fs.Var(&serving, "serving-address", "another address the API server's certificate names, repeatable")
	fs.Var(&binaries, "binary", "<service>=<absolute path>, the executable of a service in place of the image's, repeatable")
	fs.Var(&disable, "disable", "a service to leave out ("+strings.Join(layout.ServiceNames, ", ")+"), repeatable")
	fs.StringVar(&machinePrefix, "machine-prefix", "", "the IPv4 prefix mvmd gives its Machines, such as 10.200.1.0/24")
	fs.StringVar(&guest, "guest", "", "naru-guest-<version>.json, the guest manifest")
	fs.StringVar(&n.FirecrackerSHA256, "firecracker-sha256", "", "the SHA-256 of the Firecracker binary the image carries")
	fs.BoolVar(&n.PairsAsFiles, "pairs-as-files", false, "a control-plane Node carries its certificate pairs as files instead of issuing them")
	fs.StringVar(&out, "out", "", "the user-data file to write (0600: it carries keys)")
	fs.StringVar(&networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name", "address", "api-server", "machine-prefix", "guest", "firecracker-sha256", "out"); err != nil {
		return err
	}
	var err error
	if n.CA, err = pki.Load(caDir); err != nil {
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
	if err := os.WriteFile(out, userData, 0o600); err != nil {
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
