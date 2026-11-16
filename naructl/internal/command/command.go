// Package command is naructl's command line: the gen commands, topology, and the Node API's client commands.
package command

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
)

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func (l *list) Type() string { return "value" }

func require(fs *pflag.FlagSet, names ...string) error {
	set := setFlags(fs)
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}

// needs refuses each of the flags given without the flag that makes it apply, which would otherwise
// be accepted and do nothing.
func needs(fs *pflag.FlagSet, with string, flags ...string) error {
	set := setFlags(fs)
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

// authority is one of the certificate authorities gen writes: where under the --ca-dir it goes, the
// name of its certificate, and what its refusal calls it.
type authority struct {
	subdir, cn, noun string
}

var (
	clusterAuthority = authority{cn: "naru-ca", noun: "an authority"}
	apiAuthority     = authority{subdir: pki.APIDir, cn: "naru-api-ca", noun: "a Node API authority"}
	hostAuthority    = authority{subdir: pki.HostDir, cn: "naru-host-ca", noun: "a host authority"}
)

// genAuthority is gen ca, gen api-ca and gen host-ca: they differ in where the authority goes.
func genAuthority(a authority) binder {
	return func(fs *pflag.FlagSet) runner {
		where := "directory that receives ca.pem and ca.key (0600)"
		if a.subdir != "" {
			where = "the cluster's directory, which receives " + a.subdir + "/ca.pem and " + a.subdir + "/ca.key"
		}
		caDir := fs.String("ca-dir", "", where)
		return func(_ []string, stdout, _ io.Writer) error {
			if err := require(fs, "ca-dir"); err != nil {
				return err
			}
			dir := filepath.Join(*caDir, a.subdir)
			for _, name := range []string{pki.CertFile, pki.KeyFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
					return fmt.Errorf("%s already holds %s; a new one would replace it", dir, a.noun)
				}
			}
			ca, err := pki.New(a.cn)
			if err != nil {
				return err
			}
			if err := ca.Write(dir); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s; keep %s in your secret store\n", filepath.Join(dir, pki.CertFile), filepath.Join(dir, pki.KeyFile), pki.KeyFile)
			return nil
		}
	}
}

func genClient(fs *pflag.FlagSet) runner {
	caDir := fs.String("ca-dir", "", "directory of ca.pem and ca.key")
	cn := fs.String("cn", "", "the certificate's CN, the caller's identity to gaon")
	out := fs.String("out", "", "directory that receives client.pem and client.key (0600)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "cn", "out"); err != nil {
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
}

// EtcdClientCN is the subject CN of the pair gen etcd-client issues: the caller of etcd's client
// port that is gaon's snapshot writer (ADR 0024). etcd authorizes no CN, any pair the CA signed is
// accepted, so the name is for whoever reads etcd's logs.
const EtcdClientCN = "gaon-etcd-snapshot"

func genEtcdClient(fs *pflag.FlagSet) runner {
	caDir := fs.String("ca-dir", "", "directory of ca.pem and ca.key")
	out := fs.String("out", "", "directory that receives ca.crt, tls.crt and tls.key (0600), the keys of gaon's Secret etcd-snapshot-etcd-client")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "out"); err != nil {
			return err
		}
		ca, err := pki.Load(*caDir)
		if err != nil {
			return err
		}
		pair, err := ca.Client(EtcdClientCN)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		if err := pki.WriteFile(filepath.Join(*out, "tls.crt"), pair.CertPEM, 0o644); err != nil {
			return err
		}
		if err := pki.WriteFile(filepath.Join(*out, "tls.key"), pair.KeyPEM, 0o600); err != nil {
			return err
		}
		// etcd's members trust the CA and serve with certificates it signed: the one file is both the
		// CA that signs their server certificates and the CA their client certificates are checked against.
		if err := pki.WriteFile(filepath.Join(*out, "ca.crt"), ca.CertPEM, 0o644); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote %s, %s and %s for CN %s\n", filepath.Join(*out, "ca.crt"), filepath.Join(*out, "tls.crt"), filepath.Join(*out, "tls.key"), EtcdClientCN)
		return nil
	}
}

func genToken(fs *pflag.FlagSet) runner {
	caDir := fs.String("ca-dir", "", "directory of the cluster's authority, which holds tokens/")
	name := fs.String("name", "", "the Node's name, such as node-2")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "ca-dir", "name"); err != nil {
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
}

// awsTogether refuses some of the four --aws flags without the others.
func awsTogether(fs *pflag.FlagSet) error {
	names := []string{"aws-region", "aws-trust-anchor", "aws-profile", "aws-role"}
	set := setFlags(fs)
	n := 0
	for _, f := range names {
		if set[f] {
			n++
		}
	}
	if n != 0 && n != len(names) {
		return fmt.Errorf("give all of --%s, or none", strings.Join(names, ", --"))
	}
	return nil
}

// parseRegistry reads one --registry value: host=<host> and the keys ca, username, password-file
// and helper, comma-separated. The CA and the password are read from their files here, so the
// user-data carries them and the Node reads no path of the operator's.
func parseRegistry(v string) (layout.Registry, error) {
	var r layout.Registry
	var caFile, passwordFile string
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			return r, fmt.Errorf("%q is not key=value", kv)
		}
		switch k {
		case "host":
			r.Host = val
		case "ca":
			caFile = val
		case "username":
			r.Username = val
		case "password-file":
			passwordFile = val
		case "helper":
			r.Helper = val
		default:
			return r, fmt.Errorf("the key %q is not one of host, ca, username, password-file, helper", k)
		}
	}
	if r.Host == "" {
		return r, errors.New("host=<host> is required")
	}
	if r.Helper != "" && (r.Username != "" || passwordFile != "") {
		return r, fmt.Errorf("%s gives both a helper and a username or password; give one", r.Host)
	}
	if (r.Username == "") != (passwordFile == "") {
		return r, fmt.Errorf("%s needs both username and password-file", r.Host)
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return r, err
		}
		r.CA = string(b)
	}
	if passwordFile != "" {
		b, err := os.ReadFile(passwordFile)
		if err != nil {
			return r, err
		}
		r.Password = strings.TrimRight(string(b), "\r\n")
	}
	return r, nil
}
