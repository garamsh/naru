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
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// orchCommands runs the commands that talk to orch: orch, enroll-key, group, network, and host pending, admit,
// label, drain and undrain. It reports false when args are not one of them, so the other host command is left alone.
func orchCommands(args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "orch":
		return true, orchCmd(args[1:], stdout, stderr)
	case "enroll-key":
		return true, enrollKeyCmd(args[1:], stdout, stderr)
	case "group":
		return true, groupCmd(args[1:], stdout, stderr)
	case "network":
		return true, networkCmd(args[1:], stdout, stderr)
	case "host":
		if len(args) > 1 {
			switch args[1] {
			case "pending":
				return true, hostPending(args[2:], stdout, stderr)
			case "admit":
				return true, hostAdmit(args[2:], stdout, stderr)
			case "label":
				return true, hostLabel(args[2:], stdout, stderr)
			case "drain":
				return true, hostDrain("drain", args[2:], stdout, stderr)
			case "undrain":
				return true, hostDrain("undrain", args[2:], stdout, stderr)
			}
		}
	}
	return false, nil
}

// Files orch init writes.
const (
	recoveryKeyFile    = "recovery.key"
	recoveryPublicFile = "recovery.pub"
)

func orchCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("orch needs init, backup, restore, csr or issue")
	}
	switch args[0] {
	case "init":
		return orchInit(args[1:], stdout, stderr)
	case "backup":
		return orchBackup(args[1:], stdout, stderr)
	case "restore":
		return orchRestore(args[1:], stdout, stderr)
	case "csr":
		return orchCSR(args[1:], stdout, stderr)
	case "issue":
		return orchIssue(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "orch "+args[0])
}

// orchFlags are what every command that calls orch takes.
type orchFlags struct {
	fs                  *flag.FlagSet
	url, ca, id, passfn *string
	// timeout bounds a call; zero is callTimeout.
	timeout time.Duration
}

// restoreTimeout bounds the send of a snapshot, which is megabytes and is checked before orch answers.
const restoreTimeout = 10 * time.Minute

func newOrchFlags(name string, stderr io.Writer) orchFlags {
	fs := newFlags(name, stderr)
	return orchFlags{
		fs:     fs,
		url:    fs.String("orch-url", "", "orch's https URL"),
		ca:     fs.String("orch-ca", "", "the authority's certificate, PEM"),
		id:     fs.String("identity", "", "your person file, written by naru-signer"),
		passfn: fs.String("passphrase-file", "", "a file holding the passphrase of the person file"),
	}
}

func (o orchFlags) parse(args []string, positional int) ([]string, error) {
	if err := o.fs.Parse(args); err != nil {
		return nil, err
	}
	if err := require(o.fs, "orch-url", "orch-ca", "identity", "passphrase-file"); err != nil {
		return nil, err
	}
	if o.fs.NArg() != positional {
		return nil, fmt.Errorf("%s takes %d argument(s)", o.fs.Name(), positional)
	}
	return o.fs.Args(), nil
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

func orchInit(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("orch init", stderr)
	out := fs.String("out-dir", "", "the directory that receives "+recoveryKeyFile+" and "+recoveryPublicFile)
	passfn := fs.String("passphrase-file", "", "a file holding the passphrase that encrypts the recovery key")
	if err := fs.Parse(args); err != nil {
		return err
	}
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

func orchBackup(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("orch backup", stderr)
	out := o.fs.String("out", "", "also fetch the sealed snapshot orch just wrote, and write it here (0600)")
	if _, err := o.parse(args, 0); err != nil {
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

func orchRestore(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("orch restore", stderr)
	snap := fs.String("snapshot", "", "a sealed snapshot file orch wrote")
	recKey := fs.String("recovery-key", "", "the recovery key file orch init wrote")
	passfn := fs.String("passphrase-file", "", "a file holding the recovery key's passphrase")
	out := fs.String("out", "", "write the plain etcd snapshot here (0600), for etcdutl snapshot restore; required without --push")
	push := fs.String("push", "", "the https URL of a naru-orchd started with --restore: send the plain snapshot to it")
	orchCA := fs.String("orch-ca", "", "with --push, the authority's certificate, PEM")
	admin := fs.String("admin", "", "with --push, root's person file")
	adminPass := fs.String("admin-passphrase-file", "", "with --push, a file holding the person file's passphrase; defaults to --passphrase-file")
	if err := fs.Parse(args); err != nil {
		return err
	}
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

func enrollKeyCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("enroll-key needs create or revoke")
	}
	switch args[0] {
	case "create":
		o := newOrchFlags("enroll-key create", stderr)
		ttl := o.fs.Duration("ttl", 24*time.Hour, "how long the key enrolls hosts")
		if _, err := o.parse(args[1:], 0); err != nil {
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
	case "revoke":
		o := newOrchFlags("enroll-key revoke", stderr)
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("enroll-key revoke needs the key ID first")
		}
		rest := []string{args[1]}
		if _, err := o.parse(args[2:], 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.RevokeEnrollmentKey(ctx, rest[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: revoked enrollment key %s\n", rest[0])
		return nil
	}
	return fmt.Errorf("unknown command %q", "enroll-key "+args[0])
}

func hostPending(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("host pending", stderr)
	if _, err := o.parse(args, 0); err != nil {
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
	_, _ = fmt.Fprintln(tw, "HOST\tCODE\tADDRESS\tFIRST SEEN")
	for _, p := range ps {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.HostID, p.Code, p.Address, p.FirstSeen.Format(time.RFC3339))
	}
	return tw.Flush()
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

func hostAdmit(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("host admit", stderr)
	var labels list
	code := o.fs.String("code", "", "the pairing code the host shows on its console")
	region := o.fs.String("region", "", "the host's region")
	ttl := o.fs.Duration("ttl", 10*time.Minute, "how long the signed statement stays valid")
	o.fs.Var(&labels, "label", "a label, <key>=<value>, repeatable")
	// The host ID comes first; the flag package stops at the first argument that is not a flag.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("host admit needs the host ID first")
	}
	id := args[0]
	if _, err := o.parse(args[1:], 0); err != nil {
		return err
	}
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

// sameCode compares pairing codes without regard to case or the hyphen.
func sameCode(a, b string) bool {
	norm := func(s string) string { return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", "")) }
	return norm(a) == norm(b)
}

func hostLabel(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("host label", stderr)
	var labels list
	region := o.fs.String("region", "", "the host's region")
	o.fs.Var(&labels, "label", "a label, <key>=<value>, repeatable; the labels given replace the host's labels")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("host label needs the host ID first")
	}
	id := args[0]
	if _, err := o.parse(args[1:], 0); err != nil {
		return err
	}
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
