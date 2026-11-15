package command

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// Files orch init writes.
const (
	recoveryKeyFile    = "recovery.key"
	recoveryPublicFile = "recovery.pub"
)

// orchFlags are what every command that calls orch takes.
type orchFlags struct {
	fs                  *pflag.FlagSet
	url, ca, id, passfn *string
	// timeout bounds a call; zero is callTimeout.
	timeout time.Duration
}

// restoreTimeout bounds the send of a snapshot, which is megabytes and is checked before orch answers.
const restoreTimeout = 10 * time.Minute

func newOrchFlags(fs *pflag.FlagSet) orchFlags {
	return orchFlags{
		fs:     fs,
		url:    fs.String("orch-url", "", "orch's https URL"),
		ca:     fs.String("orch-ca", "", "the authority's certificate, PEM"),
		id:     fs.String("identity", "", "your person file, written by naru-signer"),
		passfn: fs.String("passphrase-file", "", "a file holding the passphrase of the person file"),
	}
}

// check refuses a command without the flags every one takes, or given a number of arguments other
// than positional.
func (o orchFlags) check(args []string, positional int) error {
	if err := require(o.fs, "orch-url", "orch-ca", "identity", "passphrase-file"); err != nil {
		return err
	}
	if len(args) != positional {
		return fmt.Errorf("%s takes %d argument(s)", o.fs.Name(), positional)
	}
	return nil
}

// passphrase reads a passphrase from a file, without its trailing newline.
func passphrase(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the passphrase file: %w", err)
	}
	p := strings.TrimRight(string(b), "\r\n")
	if p == "" {
		return "", errors.New("the passphrase file is empty")
	}
	return p, nil
}

// person opens the person file.
func (o orchFlags) person() (identity.Person, error) {
	pass, err := passphrase(*o.passfn)
	if err != nil {
		return identity.Person{}, err
	}
	data, err := os.ReadFile(*o.id)
	if err != nil {
		return identity.Person{}, fmt.Errorf("read the person file: %w", err)
	}
	return identity.DecryptPerson(pass, data)
}

func (o orchFlags) dial(p identity.Person) (*orchapi.Client, error) {
	pemBytes, err := os.ReadFile(*o.ca)
	if err != nil {
		return nil, fmt.Errorf("read the authority: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("the authority file holds no certificate")
	}
	cert := tls.Certificate{Certificate: [][]byte{p.Cert.Raw}, PrivateKey: p.Key, Leaf: p.Cert}
	return orchapi.New(*o.url, orchapi.HTTPClient(orchapi.TrustCA(pool, &cert), cmp.Or(o.timeout, callTimeout)))
}

// connect opens the person file and dials orch with it.
func (o orchFlags) connect() (identity.Person, *orchapi.Client, error) {
	p, err := o.person()
	if err != nil {
		return identity.Person{}, nil, err
	}
	cl, err := o.dial(p)
	return p, cl, err
}

func orchInit(fs *pflag.FlagSet) runner {
	out := fs.String("out-dir", "", "the directory that receives "+recoveryKeyFile+" and "+recoveryPublicFile)
	passfn := fs.String("passphrase-file", "", "a file holding the passphrase that encrypts the recovery key")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "out-dir", "passphrase-file"); err != nil {
			return err
		}
		pass, err := passphrase(*passfn)
		if err != nil {
			return err
		}
		keyPath, pubPath := filepath.Join(*out, recoveryKeyFile), filepath.Join(*out, recoveryPublicFile)
		for _, p := range []string{keyPath, pubPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s exists; a new recovery key would orphan every backup sealed to it", p)
			}
		}
		rec, err := identity.GenerateRecovery()
		if err != nil {
			return err
		}
		enc, err := rec.Encrypt(pass)
		if err != nil {
			return err
		}
		pub, err := rec.Public().MarshalPublic()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(keyPath, enc, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(pubPath, pub, 0o644); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote %s (encrypted; keep it and its passphrase apart from the control host) and %s (give it to naru-orchd as --recovery-public-key)\n", keyPath, pubPath)
		return nil
	}
}

func orchBackup(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	out := o.fs.String("out", "", "also fetch the sealed snapshot orch just wrote, and write it here (0600)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		res, err := cl.Snapshot(ctx)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: orch wrote the sealed snapshot %s (%d bytes) on its member's snapshot directory\n", res.File, res.Size)
		if *out == "" {
			return nil
		}
		sealed, err := cl.LatestSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, sealed, 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote the sealed snapshot to %s; only the recovery key opens it\n", *out)
		return nil
	}
}

func orchRestore(fs *pflag.FlagSet) runner {
	snap := fs.String("snapshot", "", "a sealed snapshot file orch wrote")
	recKey := fs.String("recovery-key", "", "the recovery key file orch init wrote")
	passfn := fs.String("passphrase-file", "", "a file holding the recovery key's passphrase")
	out := fs.String("out", "", "write the plain etcd snapshot here (0600), for etcdutl snapshot restore; required without --push")
	push := fs.String("push", "", "the https URL of a naru-orchd started with --restore: send the plain snapshot to it")
	orchCA := fs.String("orch-ca", "", "with --push, the authority's certificate, PEM")
	admin := fs.String("admin", "", "with --push, root's person file")
	adminPass := fs.String("admin-passphrase-file", "", "with --push, a file holding the person file's passphrase; defaults to --passphrase-file")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := require(fs, "snapshot", "recovery-key", "passphrase-file"); err != nil {
			return err
		}
		if *push == "" && *out == "" {
			return errors.New("--out or --push is required: the plain snapshot goes nowhere else")
		}
		if *push != "" {
			if err := require(fs, "orch-ca", "admin"); err != nil {
				return err
			}
		}
		pass, err := passphrase(*passfn)
		if err != nil {
			return err
		}
		keyData, err := os.ReadFile(*recKey)
		if err != nil {
			return fmt.Errorf("read the recovery key: %w", err)
		}
		rec, err := identity.DecryptRecovery(pass, keyData)
		if err != nil {
			return err
		}
		sealed, err := os.ReadFile(*snap)
		if err != nil {
			return fmt.Errorf("read the snapshot: %w", err)
		}
		plain, err := identity.Open(rec.X25519, sealed, []byte(orchapi.SnapshotInfo))
		if err != nil {
			return err
		}
		if *out != "" {
			if err := os.WriteFile(*out, plain, 0o600); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "naructl: wrote %s; restore it into a new member with etcdutl snapshot restore\n", *out)
		}
		if *push == "" {
			return nil
		}
		o := orchFlags{fs: fs, url: push, ca: orchCA, id: admin, passfn: adminPass, timeout: restoreTimeout}
		if *adminPass == "" {
			o.passfn = passfn
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		// A restore is many megabytes and etcd's own check runs before it answers.
		ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
		defer cancel()
		sum := sha256.Sum256(plain)
		if err := cl.Restore(ctx, bytes.NewReader(plain), hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: %s restored the snapshot (sha256 %x) and runs; start the other members with --join\n", *push, sum)
		return nil
	}
}

// enrollKeyCreate is enroll-key create.
func enrollKeyCreate(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the key enrolls hosts")
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
		k, err := cl.CreateEnrollmentKey(ctx, time.Now().Add(*ttl))
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "id      %s\nexpires %s\nsecret  %s\nThe secret is shown once; orch keeps only its hash.\n", k.ID, k.ExpiresAt.Format(time.RFC3339), k.Secret)
		return nil
	}
}

// enrollKeyRevoke is enroll-key revoke.
func enrollKeyRevoke(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.RevokeEnrollmentKey(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: revoked enrollment key %s\n", args[0])
		return nil
	}
}

// hostList lists the admitted hosts with the prefix orch allocated for each host's Machines.
func hostList(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		hs, err := cl.Hosts(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "HOST\tREGION\tADDRESS\tMACHINE PREFIX\tLAST SEEN\tLABELS")
		for _, h := range hs {
			seen := "never"
			if !h.LastSeen.IsZero() {
				seen = h.LastSeen.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", h.ID, h.Region, h.Address, cmp.Or(h.MachinePrefix, "-"), seen, labelsText(h.Labels))
		}
		return tw.Flush()
	}
}

// labelsText is the labels as key=value pairs in key order, or a dash.
func labelsText(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	slices.Sort(pairs)
	return cmp.Or(strings.Join(pairs, ","), "-")
}

func hostPending(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		ps, err := cl.Pending(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "HOST\tCODE\tADDRESS\tFIRST SEEN\tHARDWARE")
		for _, p := range ps {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.HostID, p.Code, p.Address, p.FirstSeen.Format(time.RFC3339), hardwareSummary(p.Hardware))
		}
		return tw.Flush()
	}
}

// attributes turns --label key=value pairs into a map.
func attributes(labels list) (map[string]string, error) {
	m := map[string]string{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--label %q is not <key>=<value>", l)
		}
		m[k] = v
	}
	return m, nil
}

func hostAdmit(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	var labels list
	code := o.fs.String("code", "", "the pairing code the host shows on its console")
	region := o.fs.String("region", "", "the host's region")
	ttl := o.fs.Duration("ttl", 10*time.Minute, "how long the signed statement stays valid")
	o.fs.Var(&labels, "label", "a label, <key>=<value>, repeatable")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		id := args[0]
		if err := require(o.fs, "code", "region"); err != nil {
			return err
		}
		attrs, err := attributes(labels)
		if err != nil {
			return err
		}
		person, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		pending, err := cl.Pending(ctx)
		if err != nil {
			return err
		}
		var p *orchapi.Pending
		for i := range pending {
			if pending[i].HostID == id {
				p = &pending[i]
			}
		}
		if p == nil {
			return fmt.Errorf("host %q is not pending", id)
		}
		// The code is compared against the key itself: orch's word for either is not taken.
		if !sameCode(identity.PairingCode(p.PublicKey), *code) {
			return fmt.Errorf("the code %s does not belong to host %q's key (it is %s); check the host's console", *code, id, identity.PairingCode(p.PublicKey))
		}
		sum := sha256.Sum256(p.PublicKey)
		stmt, err := identity.SignAdmission(identity.Admission{
			HostID: id, HostKeySHA256: hex.EncodeToString(sum[:]), Region: *region, Labels: attrs, ExpiresAt: time.Now().Add(*ttl),
		}, person.Key, person.Cert)
		if err != nil {
			return err
		}
		h, err := cl.Admit(ctx, orchapi.AdmitRequest{HostID: id, Code: *code, Statement: stmt, Region: *region, Labels: attrs})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: admitted host %s in region %s\n", h.ID, h.Region)
		return nil
	}
}

// sameCode compares pairing codes without regard to case or the hyphen.
func sameCode(a, b string) bool {
	norm := func(s string) string { return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", "")) }
	return norm(a) == norm(b)
}

func hostLabel(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	var labels list
	region := o.fs.String("region", "", "the host's region")
	o.fs.Var(&labels, "label", "a label, <key>=<value>, repeatable; the labels given replace the host's labels")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		id := args[0]
		if err := require(o.fs, "region"); err != nil {
			return err
		}
		attrs, err := attributes(labels)
		if err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		h, err := cl.SetLabels(ctx, id, orchapi.SetLabelsRequest{Region: *region, Labels: attrs})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: host %s is in region %s with %d label(s)\n", h.ID, h.Region, len(h.Labels))
		return nil
	}
}

// hardwareSummary is what a host reported of itself in one line, or a dash for a host that reported
// nothing (one enrolled with its own configuration).
func hardwareSummary(h *orchapi.Hardware) string {
	if h == nil {
		return "-"
	}
	return fmt.Sprintf("%d CPU, %d MiB, %d disk(s), %d link(s)", h.CPUs, h.MemoryBytes>>20, len(h.Disks), len(h.Links))
}
