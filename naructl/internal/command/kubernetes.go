package command

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

const kubernetesUsage = `  gen kubernetes --ca <dir> --endpoint <ip> [--admin-address <host:port>]
              write the front-proxy authority, the ServiceAccount keys and the administrator's kubeconfig, admin.conf, into <dir>
  gen kubernetes-node --ca <dir> --name <name> --role control-plane|worker --address <cidr> --endpoint <ip>
              --pod-network <cidr> --pod-prefix <cidr> --out <file> [--peer <address>=<pod-prefix>] [more flags: naructl gen kubernetes-node -h]
`

// genKubernetes writes what a cluster has besides its authority: the front-proxy authority, the
// ServiceAccount keys and the administrator's kubeconfig (ADR 0031). Files already there are kept.
func genKubernetes(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen kubernetes", stderr)
	caDir := fs.String("ca", "", "directory of ca.pem and ca.key, which receives the rest")
	endpoint := fs.String("endpoint", "", "the address of the API server")
	adminAddress := fs.String("admin-address", "", "the host:port admin.conf dials, when it is not the endpoint on the API port (a forward)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "endpoint"); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(*endpoint)
	if err != nil {
		return fmt.Errorf("--endpoint: %w", err)
	}
	ca, err := pki.Load(*caDir)
	if err != nil {
		return err
	}
	if _, err := loadOrMakeFrontProxy(*caDir); err != nil {
		return err
	}
	key, pub := filepath.Join(*caDir, pki.ServiceAccountKeyFile), filepath.Join(*caDir, pki.ServiceAccountPubFile)
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		keyPEM, pubPEM, err := pki.ServiceAccountKey()
		if err != nil {
			return err
		}
		if err := pki.WriteFile(key, keyPEM, 0o600); err != nil {
			return err
		}
		if err := pki.WriteFile(pub, pubPEM, 0o644); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	admin, err := ca.Client(pki.AdminName, pki.AdminOrganization)
	if err != nil {
		return err
	}
	conf := filepath.Join(*caDir, pki.AdminKubeconfigFile)
	server := "https://" + net.JoinHostPort(addr.String(), layout.APIPort)
	if *adminAddress != "" {
		server = "https://" + *adminAddress
	}
	if err := pki.WriteFile(conf, pki.Kubeconfig(pki.AdminName, server, ca.CertPEM, admin), 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s, which carries a key; keep it in your secret store\n", conf)
	return nil
}

// loadOrMakeFrontProxy returns the front-proxy authority of the cluster directory, making it first if there is none.
func loadOrMakeFrontProxy(caDir string) (*pki.CA, error) {
	dir := filepath.Join(caDir, pki.FrontProxyDir)
	if _, err := os.Stat(filepath.Join(dir, pki.KeyFile)); err == nil {
		return pki.Load(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ca, err := pki.New("naru-front-proxy-ca")
	if err != nil {
		return nil, err
	}
	return ca, ca.Write(dir)
}

func genKubernetesNode(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen kubernetes-node", stderr)
	var (
		nameservers, binaries, disable, timeServers, peers list
		extraNIC                                           string
		n                                                  layout.Node
		k                                                  layout.KubeNode
		caDir, role, address, endpoint, podNetwork         string
		podPrefix, gateway, out, networkConfig             string
	)
	fs.StringVar(&caDir, "ca", "", "directory of ca.pem and ca.key, and what gen kubernetes wrote there")
	fs.StringVar(&n.Name, "name", "", "the Node's name, such as node-1; it is the Kubernetes Node's name")
	fs.StringVar(&role, "role", "", "control-plane (etcd and the control-plane programs besides the kubelet) or worker")
	fs.StringVar(&address, "address", "", "the Node's address and prefix, such as 192.168.0.40/24")
	fs.StringVar(&endpoint, "endpoint", "", "the address of the API server, which the kubelet and kube-proxy dial: the control-plane Node's")
	fs.StringVar(&podNetwork, "pod-network", "", "the prefix every Pod address is in, such as 10.64.0.0/12")
	fs.StringVar(&podPrefix, "pod-prefix", "", "the part of the Pod network this Node's Pods take, such as 10.64.1.0/24")
	fs.Var(&peers, "peer", "another Node, <address>=<pod-prefix>, repeatable: the Node routes to the prefix via the address")
	fs.StringVar(&n.MAC, "mac", "", "the MAC address of the Node's card (with --network-config)")
	fs.StringVar(&gateway, "gateway", "", "the default gateway (with --network-config)")
	fs.Var(&nameservers, "nameserver", "a nameserver, repeatable (with --network-config)")
	fs.StringVar(&n.Search, "search", "", "the search domain (with --network-config)")
	fs.StringVar(&extraNIC, "extra-nic", "", "a second card, <mac>=<cidr>, with no gateway (with --network-config)")
	fs.Var(&binaries, "binary", "<service>=<absolute path>, the executable of a service in place of the image's, repeatable")
	fs.Var(&disable, "disable", "a service to leave out ("+strings.Join(layout.Kubernetes.Names(), ", ")+"), repeatable")
	fs.Var(&timeServers, "time-server", "an SNTP server init sets the clock from, host or host:port, repeatable")
	fs.BoolVar(&k.LocalRegistry, "local-registry", false, "run the registry service from the data disk's inputs")
	fs.StringVar(&k.RegistryCA, "registry-ca", "", "the path on the Node of the CA containerd trusts for the registry service (with --sandbox-image)")
	fs.StringVar(&k.SandboxImage, "sandbox-image", "", "the image of a Pod's sandbox container, which containerd pulls")
	fs.StringVar(&out, "out", "", "the user-data file to write (0600: it carries keys; a file already there is replaced)")
	fs.StringVar(&networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name", "role", "address", "endpoint", "pod-network", "pod-prefix", "out"); err != nil {
		return err
	}
	if err := needs(fs, "network-config", "mac", "gateway", "nameserver", "search", "extra-nic"); err != nil {
		return err
	}
	switch role {
	case "control-plane":
		k.ControlPlane = true
	case "worker":
	default:
		return fmt.Errorf("--role is %q, not control-plane or worker", role)
	}
	var err error
	if n.CA, err = pki.Load(caDir); err != nil {
		return err
	}
	if k.ControlPlane {
		if k.FrontProxy, err = pki.Load(filepath.Join(caDir, pki.FrontProxyDir)); err != nil {
			return fmt.Errorf("the front-proxy authority (run gen kubernetes): %w", err)
		}
		if k.ServiceAccountKey, err = os.ReadFile(filepath.Join(caDir, pki.ServiceAccountKeyFile)); err != nil {
			return fmt.Errorf("the ServiceAccount key (run gen kubernetes): %w", err)
		}
		if k.ServiceAccountPub, err = os.ReadFile(filepath.Join(caDir, pki.ServiceAccountPubFile)); err != nil {
			return fmt.Errorf("the ServiceAccount key (run gen kubernetes): %w", err)
		}
	}
	for _, p := range []struct {
		flag string
		to   *netip.Prefix
		from string
	}{{"address", &n.Address, address}, {"pod-network", &k.PodNetwork, podNetwork}, {"pod-prefix", &k.PodPrefix, podPrefix}} {
		if *p.to, err = netip.ParsePrefix(p.from); err != nil {
			return fmt.Errorf("--%s: %w", p.flag, err)
		}
	}
	if n.APIServer, err = netip.ParseAddr(endpoint); err != nil {
		return fmt.Errorf("--endpoint: %w", err)
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
	for _, s := range peers {
		addr, prefix, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--peer %q is not <address>=<pod-prefix>", s)
		}
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		k.Peers = append(k.Peers, layout.KubePeer{Address: a, PodPrefix: p})
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
	n.Disable, n.TimeServers, n.Kube = disable, timeServers, &k
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
