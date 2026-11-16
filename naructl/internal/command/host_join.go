package command

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/naructl/internal/seed"
)

// installSeedMode is the mode of the seed: its user-data carries the enrollment key.
const installSeedMode = 0o600

// hostInstallSeed is host install-seed: it writes the ISO 9660 NoCloud volume a host boots the
// installer with. The enrollment key is read from a file, never taken from a flag, so it is in no
// shell history and no process list.
func hostInstallSeed(fs *pflag.FlagSet) runner {
	orchURL := fs.String("orch-url", "", "orch's https URL, which the host enrolls with")
	orchCA := fs.String("orch-ca-sha256", "", "the hex SHA-256 of the authority orch's certificate chains to")
	keyFile := fs.String("enroll-key", "", "a file holding the enrollment key, which enroll-key create printed")
	name := fs.String("name", "", "the name the host is listed under until it is admitted, a DNS label (default none: its ID)")
	out := fs.String("out", "", "the ISO to write (0600: it carries the enrollment key; a file already there is replaced)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "orch-url", "orch-ca-sha256", "enroll-key", "out"); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("host install-seed takes no argument")
		}
		secret, err := readText("--enroll-key", *keyFile)
		if err != nil {
			// The error names the path, and a key typed where a file belongs would be echoed with it.
			return errors.New("--enroll-key takes a file that holds the enrollment key, and that file could not be read")
		}
		img, err := seed.Join{OrchURL: *orchURL, CASHA256: *orchCA, EnrollKey: strings.TrimSpace(secret), Name: *name}.ISO()
		if err != nil {
			return err
		}
		if err := pki.WriteFile(*out, img, installSeedMode); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote %s; attach it to the host as a CD or a disk and boot the installer from the naru-host image\n", *out)
		return nil
	}
}

// genHostProfile is gen host-profile: the fleet-wide inputs of a joined host, which orch keeps and
// builds each host's configuration from. It takes gen node's flags for what every host shares and
// none for what is one host's own.
func genHostProfile(fs *pflag.FlagSet) runner {
	var (
		p                                                   layout.HostProfile
		timeServers, registries, storage, extensions        list
		awsRegion, awsTrustAnchor, awsProfile, awsRole      string
		hostMgmt, hostEgress, apiClientCA, guest, pool, out string
		hostTunnelPort                                      int
	)
	fs.StringVar(&guest, "guest", "", "naru-guest-<version>.json, the guest manifest")
	fs.StringVar(&p.FirecrackerSHA256, "firecracker-sha256", "", "the SHA-256 of the Firecracker binary the image carries")
	fs.Var(&timeServers, "time-server", "an SNTP server init sets the clock from, host or host:port, repeatable and asked in order")
	fs.Var(&registries, "registry", "a registry mvmd pulls from, host=<host[:port]> then any of ca=<PEM file>, username=<name> with password-file=<file>, or helper=<name>, comma-separated, repeatable; the profile then carries the password, so its file is 0600")
	fs.StringVar(&awsRegion, "aws-region", "", "the AWS region of the IAM Roles Anywhere profile the credential helpers use (with the other --aws flags: all four or none)")
	fs.StringVar(&awsTrustAnchor, "aws-trust-anchor", "", "the ARN of the IAM Roles Anywhere trust anchor")
	fs.StringVar(&awsProfile, "aws-profile", "", "the ARN of the IAM Roles Anywhere profile")
	fs.StringVar(&awsRole, "aws-role", "", "the ARN of the IAM role; the host's certificate and key are the pair")
	fs.StringVar(&p.MetricsListen, "metrics-listen", "", "spec.metrics: the IP address init and the daemons serve /metrics on")
	fs.StringVar(&hostMgmt, "host-management-prefix", "", "spec.host.network: the prefix of the management plane, which no virtual network may reach, such as 192.168.0.0/24")
	fs.StringVar(&hostEgress, "host-egress-interface", "", "spec.host.network: the interface the virtual networks' egress leaves by (with --host-management-prefix)")
	fs.IntVar(&hostTunnelPort, "host-tunnel-port", 0, "spec.host.network: the UDP port of the WireGuard tunnel (with --host-management-prefix; default 51820)")
	fs.Var(&storage, "storage", "<name>=<label, /dev/path or serial:NUMBER>, an extra disk the host offers as XFS, repeatable: the first holds mvmd's disks, snapshots and Machine run directories, on XFS with reflinks, and the others are mounted under /var/lib/naru/storage")
	fs.Var(&extensions, "extension", "<name>=<SHA-256 of the image>, a system extension init mounts before any service starts, repeatable")
	fs.StringVar(&apiClientCA, "api-client-ca", "", "the file holding the PEM of the authority naru-hostd's clients chain to, beside orch's (a key in it is refused)")
	fs.StringVar(&pool, "machine-pool", "", "the IPv4 prefix orch gives each host a /24 of for its Machines, such as 10.200.0.0/16")
	fs.StringVar(&p.Orch.URL, "orch-url", "", "orch's https URL, which an admitted host's agent keeps reporting to")
	fs.StringVar(&p.Orch.CASHA256, "orch-ca-sha256", "", "the hex SHA-256 of the authority orch's certificate chains to")
	fs.StringVar(&out, "out", "", "the profile to write (0600 when it carries a registry's password, else 0644)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "guest", "firecracker-sha256", "machine-pool", "orch-url", "orch-ca-sha256", "out"); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("gen host-profile takes no argument")
		}
		if err := awsTogether(fs); err != nil {
			return err
		}
		if err := needs(fs, "host-management-prefix", "host-egress-interface", "host-tunnel-port"); err != nil {
			return err
		}
		var err error
		if p.Guest, err = layout.ReadManifest(guest); err != nil {
			return err
		}
		if p.MachinePool, err = netip.ParsePrefix(pool); err != nil {
			return fmt.Errorf("--machine-pool: %w", err)
		}
		p.TimeServers = timeServers
		for _, r := range registries {
			reg, err := parseRegistry(r)
			if err != nil {
				return fmt.Errorf("--registry: %w", err)
			}
			p.Registries = append(p.Registries, reg)
		}
		if awsRegion != "" {
			p.AWS = &layout.AWS{Region: awsRegion, TrustAnchorArn: awsTrustAnchor, ProfileArn: awsProfile, RoleArn: awsRole}
		}
		if hostMgmt != "" {
			prefix, err := netip.ParsePrefix(hostMgmt)
			if err != nil {
				return fmt.Errorf("--host-management-prefix: %w", err)
			}
			p.Network = &layout.HostNetwork{ManagementPrefix: prefix, EgressInterface: hostEgress, TunnelPort: hostTunnelPort}
		}
		for _, s := range storage {
			d, err := parseStorage(s)
			if err != nil {
				return err
			}
			p.Storage = append(p.Storage, d)
		}
		for _, e := range extensions {
			x, err := parseExtension(e)
			if err != nil {
				return err
			}
			p.Extensions = append(p.Extensions, x)
		}
		if apiClientCA != "" {
			if p.APIClientCA, err = readText("--api-client-ca", apiClientCA); err != nil {
				return err
			}
		}
		if err := p.Check(); err != nil {
			return err
		}
		b, err := p.Marshal()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if p.HasCredentials() {
			mode = 0o600
		}
		if err := pki.WriteFile(out, b, mode); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote %s (mode %04o); load it with host profile set -f\n", out, mode)
		return nil
	}
}

// parseStorage reads one --storage value, <name>=<label, /dev/path or serial:NUMBER>.
func parseStorage(s string) (layout.StorageDisk, error) {
	name, value, ok := strings.Cut(s, "=")
	if !ok {
		return layout.StorageDisk{}, fmt.Errorf("--storage %q is not <name>=<label, /dev/path or serial:NUMBER>", s)
	}
	if serial, ok := strings.CutPrefix(value, "serial:"); ok {
		if serial == "" {
			return layout.StorageDisk{}, fmt.Errorf("--storage %q names no serial number", s)
		}
		return layout.StorageDisk{Name: name, Serial: serial}, nil
	}
	if strings.HasPrefix(value, "/dev/") {
		return layout.StorageDisk{Name: name, Device: value}, nil
	}
	return layout.StorageDisk{Name: name, Label: value}, nil
}

// parseExtension reads one --extension value, <name>=<SHA-256 of the image>.
func parseExtension(e string) (layout.Extension, error) {
	name, digest, ok := strings.Cut(e, "=")
	if !ok || name == "" || digest == "" {
		return layout.Extension{}, fmt.Errorf("--extension %q is not <name>=<SHA-256 of the image>", e)
	}
	return layout.Extension{Name: name, SHA256: digest}, nil
}

// hostProfileSet is host profile set.
func hostProfileSet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	file := fs.StringP("file", "f", "", "the profile, written by gen host-profile")
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if err := require(o.fs, "file"); err != nil {
			return err
		}
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		if _, err := layout.ParseHostProfile(b); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.SetProfile(ctx, b); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: orch keeps the host profile; hosts admitted from now on are built from it\n")
		return nil
	}
}

// hostProfileGet is host profile get.
func hostProfileGet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		p, err := cl.Profile(ctx)
		if err != nil {
			return err
		}
		_, err = stdout.Write(p.Profile)
		return err
	}
}

// hostRemove is host remove: orch forgets the host and takes back its Machine prefix. It does not
// revoke the host's certificate; naructl operator revoke does that (ADR 0059).
func hostRemove(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		id := args[0]
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.Remove(ctx, id); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: removed host %s and freed its Machine prefix; its certificate is still valid until it expires: revoke it with naructl operator revoke\n", id)
		return nil
	}
}
