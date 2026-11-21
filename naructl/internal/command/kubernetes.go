package command

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

// genKubernetes writes what a cluster has besides its authority: the front-proxy authority, the
// ServiceAccount keys and the administrator's kubeconfig (ADR 0031). Files already there are kept.
func genKubernetes(fs *pflag.FlagSet) runner {
	caDir := fs.String("ca-dir", "", "directory of ca.pem and ca.key, which receives the rest")
	endpoint := fs.String("endpoint", "", "the address of the API server")
	adminAddress := fs.String("admin-address", "", "the host:port admin.conf dials, when it is not the endpoint on the API port (a forward)")
	disableAddons := fs.StringArray("disable-addon", nil, "a cluster add-on the cluster does not run ("+strings.Join(layout.Addons, ", ")+"), repeatable: its objects are the cluster owner's, and the Nodes neither apply nor remove them")
	issuers := fs.StringArray("service-account-issuer", nil, "a ServiceAccount issuer, an https URL, repeatable: the first signs the tokens and the API server accepts them all and the in-cluster issuer (ADR 0087)")
	algorithm := fs.String("service-account-algorithm", string(pki.DefaultAlgorithm), "the algorithm of the ServiceAccount key made: RS256 or ES256; a key already there is kept, and an algorithm named that differs from its refuses")
	rotate := fs.String("rotate-service-account-key", "", "make a new ServiceAccount key that signs, and keep the public key of the one it replaces so that its tokens stay valid; the value is the key id (kid) of the signing key being replaced, which gen kubernetes-jwks prints, so that a run repeated after it succeeded is refused, not applied twice")
	dropPrevious := fs.Bool("drop-previous-service-account-key", false, "drop every ServiceAccount public key but the signing key's, whose tokens stop being valid (a previous key that leaked), without making a new key")
	clearIssuers := fs.Bool("clear-service-account-issuers", false, "remove the ServiceAccount issuers, so that the in-cluster issuer alone signs; without this and without --service-account-issuer, the issuers there are kept")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "endpoint"); err != nil {
			return err
		}
		alg, err := pki.ParseAlgorithm(*algorithm)
		if err != nil {
			return fmt.Errorf("--service-account-algorithm: %w", err)
		}
		if !layout.ValidIssuers(*issuers) {
			return errors.New("--service-account-issuer is not a plain https URL (a host, a path of letters, digits and - . _ ~ segments, no userinfo, query, fragment, trailing slash or space), or is repeated")
		}
		if *clearIssuers && len(*issuers) > 0 {
			return errors.New("--clear-service-account-issuers and --service-account-issuer are exclusive")
		}
		if *dropPrevious && fs.Changed("rotate-service-account-key") {
			return errors.New("--drop-previous-service-account-key and --rotate-service-account-key are exclusive")
		}
		if fs.Changed("rotate-service-account-key") && *rotate == "" {
			return errors.New("--rotate-service-account-key names the key id of the signing key it replaces")
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
		for _, name := range *disableAddons {
			if !slices.Contains(layout.Addons, name) {
				return fmt.Errorf("--disable-addon %q is not one of %s", name, strings.Join(layout.Addons, ", "))
			}
		}
		// The file follows the flags: with none, every add-on is on again.
		disabled := filepath.Join(*caDir, pki.DisabledAddonsFile)
		if len(*disableAddons) == 0 {
			if err := os.Remove(disabled); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err := pki.WriteFile(disabled, []byte(strings.Join(*disableAddons, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		// Unlike the add-ons, the issuers are kept when the flag is absent: dropping them would break the
		// federation of a cluster silently (ADR 0087).
		issuersFile := filepath.Join(*caDir, pki.ServiceAccountIssuersFile)
		switch {
		case *clearIssuers:
			if err := os.Remove(issuersFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		case len(*issuers) > 0:
			if err := pki.WriteFile(issuersFile, []byte(strings.Join(*issuers, "\n")+"\n"), 0o644); err != nil {
				return err
			}
		}
		if err := serviceAccountKey(*caDir, saKeyOptions{alg: alg, named: fs.Changed("service-account-algorithm"), rotateKid: *rotate, dropPrevious: *dropPrevious}); err != nil {
			return err
		}
		admin, err := ca.Client(pki.AdminName, pki.AdminOrganization)
		if err != nil {
			return err
		}
		conf := filepath.Join(*caDir, pki.AdminKubeconfigFile)
		server := "https://" + net.JoinHostPort(addr.String(), wire.KubeAPIPort)
		if *adminAddress != "" {
			server = "https://" + *adminAddress
		}
		if err := pki.WriteFile(conf, pki.Kubeconfig(pki.AdminName, server, ca.CertPEM, admin), 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote %s, which carries a key; keep it in your secret store\n", conf)
		return nil
	}
}

// writeKeyFile writes a key file; a test replaces it to end a run between the two files.
var writeKeyFile = pki.WriteFile

// saKeyOptions say what serviceAccountKey does to the key pair.
type saKeyOptions struct {
	alg   pki.Algorithm
	named bool
	// rotateKid, when set, replaces the signing key whose key id it is.
	rotateKid string
	// dropPrevious keeps only the signing key's public key.
	dropPrevious bool
}

// serviceAccountKey makes the cluster's ServiceAccount key pair in caDir if there is none. A key there
// is kept unless o.rotateKid names its id, which makes a new one that signs while the public key of the
// one it replaces stays in sa.pub (ADR 0087), or o.dropPrevious drops the keys after the signing one. A
// key there of another algorithm than one the operator named is refused. The signing key is the one
// sa.key holds: a rotation writes sa.pub first, so a run that ends between the two files leaves a pair
// that verifies.
func serviceAccountKey(caDir string, o saKeyOptions) error {
	key, pub := filepath.Join(caDir, pki.ServiceAccountKeyFile), filepath.Join(caDir, pki.ServiceAccountPubFile)
	current, err := os.ReadFile(key)
	if errors.Is(err, os.ErrNotExist) {
		if o.rotateKid != "" || o.dropPrevious {
			return errors.New("the cluster has no ServiceAccount key to rotate or drop the previous key of")
		}
		keyPEM, pubPEM, err := pki.ServiceAccountKey(o.alg)
		if err != nil {
			return err
		}
		// The key first: a run that ends between the files leaves no sa.pub, and the next makes both.
		if err := writeKeyFile(key, keyPEM, 0o600); err != nil {
			return err
		}
		return writeKeyFile(pub, pubPEM, 0o644)
	}
	if err != nil {
		return err
	}
	signer, err := pki.SigningPublicKey(current)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	have, err := pki.KeyAlgorithm(signer)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	switch {
	case o.dropPrevious:
		return writeKeyFile(pub, signer, 0o644)
	case o.rotateKid != "":
		kid, err := pki.SigningKeyID(signer)
		if err != nil {
			return err
		}
		if kid != o.rotateKid {
			return fmt.Errorf("the signing ServiceAccount key is %s, not %s; it may be rotated already, and a second rotation would drop the key that signed the tokens in use", kid, o.rotateKid)
		}
		// A rotation that names no algorithm keeps the one the cluster has.
		alg := o.alg
		if !o.named {
			alg = have
		}
		keyPEM, pubPEM, err := pki.ServiceAccountKey(alg)
		if err != nil {
			return err
		}
		if pubPEM, err = pki.RotatedPublicKeys(pubPEM, signer); err != nil {
			return err
		}
		if err := writeKeyFile(pub, pubPEM, 0o644); err != nil {
			return err
		}
		return writeKeyFile(key, keyPEM, 0o600)
	case o.named && have != o.alg:
		return fmt.Errorf("the ServiceAccount key is %s, not %s; --rotate-service-account-key makes a new one", have, o.alg)
	}
	return nil
}

// genKubernetesJWKS writes what an external OpenID Connect provider's client fetches from the first
// issuer: the discovery document and the JSON Web Key Set of the ServiceAccount public keys, under the
// paths the issuer serves them at, for the operator to publish as they are (ADR 0087).
func genKubernetesJWKS(fs *pflag.FlagSet) runner {
	caDir := fs.String("ca-dir", "", "directory gen kubernetes wrote the ServiceAccount keys and issuers to")
	outDir := fs.String("out-dir", "", "the directory that receives .well-known/openid-configuration and openid/v1/jwks, under the issuer's path")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "out-dir"); err != nil {
			return err
		}
		issuers, err := readIssuers(*caDir)
		if err != nil {
			return err
		}
		if len(issuers) == 0 {
			return errors.New("the cluster has no ServiceAccount issuer to publish for (run gen kubernetes --service-account-issuer)")
		}
		pub, err := os.ReadFile(filepath.Join(*caDir, pki.ServiceAccountPubFile))
		if err != nil {
			return fmt.Errorf("the ServiceAccount key (run gen kubernetes): %w", err)
		}
		issuer := issuers[0]
		base, err := issuerDir(*outDir, issuer)
		if err != nil {
			return err
		}
		kid, err := pki.SigningKeyID(pub)
		if err != nil {
			return fmt.Errorf("%s: %w", pki.ServiceAccountPubFile, err)
		}
		_, _ = fmt.Fprintf(stdout, "naructl: the signing ServiceAccount key id is %s (--rotate-service-account-key takes it)\n", kid)
		discovery, err := pki.OIDCDiscovery(issuer, layout.JWKSURI(issuer), pub)
		if err != nil {
			return err
		}
		jwks, err := pki.ServiceAccountJWKS(pub)
		if err != nil {
			return err
		}
		for rel, content := range map[string][]byte{
			".well-known/openid-configuration":       discovery,
			strings.TrimPrefix(layout.JWKSPath, "/"): jwks,
		} {
			file := filepath.Join(base, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			if err := pki.WriteFile(file, append(content, '\n'), 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "naructl: wrote %s, to serve at %s/%s\n", file, issuer, rel)
		}
		return nil
	}
}

// issuerDir returns the directory under outDir that serves issuer's path, and refuses one that is not
// inside outDir.
func issuerDir(outDir, issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(outDir, filepath.FromSlash(strings.TrimPrefix(u.Path, "/")))
	rel, err := filepath.Rel(outDir, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("the issuer %q names a path outside --out-dir", issuer)
	}
	return dir, nil
}

// readIssuers returns the issuers gen kubernetes wrote to caDir, in order, and refuses a file that
// holds one gen kubernetes would not have written.
func readIssuers(caDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(caDir, pki.ServiceAccountIssuersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	issuers := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if !layout.ValidIssuers(issuers) {
		return nil, fmt.Errorf("%s holds an issuer that is not a plain https URL", pki.ServiceAccountIssuersFile)
	}
	return issuers, nil
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

func genKubernetesNode(fs *pflag.FlagSet) runner {
	var (
		nameservers, binaries, disable, timeServers, peers list
		storage, blockSerials                              list
		extraNIC                                           string
		n                                                  layout.Node
		k                                                  layout.KubeNode
		caDir, role, address, endpoint, podNetwork         string
		podPrefix, gateway, out, networkConfig             string
	)
	fs.StringVar(&caDir, "ca-dir", "", "directory of ca.pem and ca.key, and what gen kubernetes wrote there")
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
	fs.Var(&storage, "storage", "an extra disk init formats as XFS and mounts, <name>=<label, /dev/path or serial:NUMBER>, repeatable: the first holds the Node's local volumes (naru-local)")
	fs.Var(&blockSerials, "block-serial", "the serial number of a disk the Node offers whole as a block volume (naru-block), repeatable: init neither formats nor mounts it")
	fs.BoolVar(&k.LocalRegistry, "local-registry", false, "run the registry service from the data disk's inputs")
	fs.StringVar(&k.RegistryCA, "registry-ca", "", "the path on the Node of the CA containerd trusts for the registry service (with --sandbox-image)")
	fs.StringVar(&k.SandboxImage, "sandbox-image", "", "the image of a Pod's sandbox container, which containerd pulls")
	fs.BoolVar(&k.BootstrapTokens, "bootstrap-tokens", false, "a control-plane Node accepts bootstrap tokens from <ca-dir>/tokens, which naructl token create adds to live")
	tokenMode := fs.Bool("token", false, "a worker's kubelet presents the token <ca-dir>/tokens/<name> for its client certificate (naructl token create --name makes it)")
	fs.StringVar(&out, "out", "", "the user-data file to write (0600: it carries keys; a file already there is replaced)")
	fs.StringVar(&networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "name", "role", "address", "endpoint", "pod-network", "pod-prefix", "out"); err != nil {
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
		// A Node API authority in the directory gives the Node spec.api, as gen node does.
		if _, serr := os.Stat(filepath.Join(caDir, pki.APIDir)); serr == nil {
			if n.API, err = pki.Load(filepath.Join(caDir, pki.APIDir)); err != nil {
				return fmt.Errorf("the Node API authority: %w", err)
			}
		} else if !errors.Is(serr, os.ErrNotExist) {
			return serr
		}
		if b, rerr := os.ReadFile(filepath.Join(caDir, pki.DisabledAddonsFile)); rerr == nil {
			k.DisabledAddons = strings.Fields(string(b))
		} else if !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		if k.ServiceAccountIssuers, err = readIssuers(caDir); err != nil {
			return err
		}
		if k.BootstrapTokens && k.ControlPlane {
			if k.Tokens, err = pki.ReadTokens(caDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if *tokenMode {
			if k.Token, err = pki.ReadToken(caDir, n.Name); err != nil {
				return fmt.Errorf("the token of %s (run token create): %w", n.Name, err)
			}
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
		for _, s := range storage {
			d, err := parseStorage(s)
			if err != nil {
				return err
			}
			n.Storage = append(n.Storage, d)
		}
		k.BlockSerials = blockSerials
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
			if err := os.WriteFile(networkConfig, nc, 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, " and %s", networkConfig)
		}
		_, _ = fmt.Fprintln(stdout)
		return nil
	}
}

// kubernetesBootstrapRBAC prints the manifest that lets a bootstrap token's holder have a kubelet's
// client certificate issued, for the administrator to apply once to a cluster that takes tokens.
func kubernetesBootstrapRBAC(fs *pflag.FlagSet) runner {
	return func(args []string, stdout, stderr io.Writer) error {
		if err := positional(args, 0); err != nil {
			return err
		}
		_, err := io.WriteString(stdout, layout.BootstrapRBAC)
		return err
	}
}
