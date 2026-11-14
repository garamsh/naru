package command

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/garamsh/naru/lib/identity"
)

// signerRequestHeader carries a signed request on the GET that has no body.
const signerRequestHeader = "X-Naru-Request"

const signerUsage = `usage: naructl signer authorise-replica --admin <person file> --passphrase-file <file> --host-id <id> --join-key <file> --out <file>
       naructl signer bundle --admin <person file> --passphrase-file <file> --socket <path> --out <file>
       naructl signer restore --admin <person file> --passphrase-file <file> --bundle <file> --recovery-key <path> --recovery-passphrase-file <path> --socket <path>

  authorise-replica signs root's authorisation for the signer of a host to receive the authorities: pass the
  file to naru-signer join. bundle writes the authorities sealed to the recovery key. restore fills a blank
  signer from that bundle; the recovery key and its passphrase file are paths on the signer's host.
  All three are root's.
`

func signerCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, signerUsage)
		return errors.New("signer authorise-replica, bundle or restore is required")
	}
	switch args[0] {
	case "authorise-replica":
		return signerAuthoriseReplica(args[1:], stdout, stderr)
	case "bundle":
		return signerBundle(args[1:], stdout, stderr)
	case "restore":
		return signerRestore(args[1:], stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, signerUsage)
	return fmt.Errorf("signer %q: authorise-replica, bundle or restore", args[0])
}

func signerAuthoriseReplica(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("signer authorise-replica", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host ID of the new signer")
	joinKey := fs.String("join-key", "", "the public half naru-signer join-key wrote on the new signer's host")
	out := fs.String("out", "", "the file the signed authorisation is written to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "host-id", "join-key", "out"); err != nil {
		return err
	}
	pub, err := os.ReadFile(*joinKey)
	if err != nil {
		return err
	}
	signed, err := signAs(*admin, *pass, identity.ActionReplicate, identity.ReplicateRequest{HostID: *hostID, X25519: pub})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, signed, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote root's authorisation for the signer of %s to %s; it expires in %s\n", *hostID, *out, operatorRequestTTL)
	return nil
}

func signerBundle(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("signer bundle", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	socket := fs.String("socket", "", "naru-signer's unix socket")
	out := fs.String("out", "", "the file the sealed bundle is written to (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "socket", "out"); err != nil {
		return err
	}
	signed, err := signAs(*admin, *pass, identity.ActionBundle, struct{}{})
	if err != nil {
		return err
	}
	bundle, err := signerDo(*socket, http.MethodGet, "/v1/bundle", nil, map[string]string{signerRequestHeader: base64.StdEncoding.EncodeToString(signed)})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, bundle, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote the sealed bundle %s (%d bytes); only the recovery key opens it\n", *out, len(bundle))
	return nil
}

func signerRestore(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("signer restore", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	bundleFile := fs.String("bundle", "", "the sealed bundle signer bundle wrote")
	recKey := fs.String("recovery-key", "", "the recovery key file, as a path on the signer's host")
	recPass := fs.String("recovery-passphrase-file", "", "the file holding its passphrase, as a path on the signer's host")
	socket := fs.String("socket", "", "naru-signer's unix socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "bundle", "recovery-key", "recovery-passphrase-file", "socket"); err != nil {
		return err
	}
	bundle, err := os.ReadFile(*bundleFile)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bundle)
	signed, err := signAs(*admin, *pass, identity.ActionRestore, identity.RestoreRequest{BundleSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	body, err := json.Marshal(identity.RestoreBody{Bundle: bundle, Authorisation: signed, RecoveryKey: *recKey, PassphraseFile: *recPass})
	if err != nil {
		return err
	}
	if _, err := signerCall(*socket, http.MethodPost, "/v1/restore", body); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: the signer restored the authorities from %s\n", *bundleFile)
	return nil
}
