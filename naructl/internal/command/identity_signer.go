package command

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// signerRequestHeader carries a signed request on the GET that has no body.
const signerRequestHeader = "X-Naru-Request"

const signerUsage = `usage: naructl signer ca [--authority orch|operator|host] (--socket <path> | --config <file> [--node <ip[:port]>])
       naructl signer authorise-replica --admin <person file> --passphrase-file <file> --host-id <id> --join-key <file> --out <file>
       naructl signer bundle --admin <person file> --passphrase-file <file> (--socket <path> | --config <file> [--node <ip[:port]>]) --out <file>
       naructl signer reissue-root --recovery-key <file> --passphrase-file <file> --csr <file> (--socket <path> | --config <file> [--node <ip[:port]>]) --out <file>
       naructl signer restore --admin <person file> --passphrase-file <file> --bundle <file> --recovery-key <path> --recovery-passphrase-file <path> (--socket <path> | --config <file> [--node <ip[:port]>])

  ca prints the certificates of the signer's authorities as PEM, each after a comment line with the
  SHA-256 of its DER, which is what a seed's --orch-ca-sha256 takes for the orch authority. It needs
  no person file: the certificates are public.
  authorise-replica signs root's authorisation for the signer of a host to receive the authorities: pass the
  file to naru-signer join. bundle writes the authorities sealed to the recovery key. restore fills a blank
  signer from that bundle; the recovery key and its passphrase file are paths on the signer's host.
  Those three are root's. reissue-root is the recovery key's, for an operator who lost root's person file:
  it signs on this machine a once-only request for a new root certificate for the request --csr names,
  and the signer revokes the old root. Its --recovery-key and --passphrase-file are local files.
  Each takes exactly one of --socket, the one naru-signer serves on this host, and --config, an admin's client
  file for a Node's API, which forwards to the signer of --node. The signer checks every request's signature
  itself, so the client file alone does not authorise anything.
`

func signerCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, signerUsage)
		return errors.New("signer ca, authorise-replica, bundle, reissue-root or restore is required")
	}
	switch args[0] {
	case "ca":
		return signerCA("signer ca", args[1:], stdout, stderr, "orch", "operator", "host")
	case "authorise-replica":
		return signerAuthoriseReplica(args[1:], stdout, stderr)
	case "bundle":
		return signerBundle(args[1:], stdout, stderr)
	case "reissue-root":
		return signerReissueRoot(args[1:], stdout, stderr)
	case "restore":
		return signerRestore(args[1:], stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, signerUsage)
	return fmt.Errorf("signer %q: ca, authorise-replica, bundle, reissue-root or restore", args[0])
}

// signerCA is signer ca and orch ca: it reads GET /v1/ca from the signer, which answers without a
// signature, and prints the certificates of the authorities named in offered, in that order, as
// PEM each after a comment line that names it and gives the SHA-256 of its DER. --authority narrows
// the output to one of offered.
func signerCA(name string, args []string, stdout, stderr io.Writer, offered ...string) error {
	fs := newFlags(name, stderr)
	authority := fs.String("authority", "", "print only this authority: "+strings.Join(offered, ", "))
	sg := addSignerFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sg.check(fs); err != nil {
		return err
	}
	if *authority != "" && !slices.Contains(offered, *authority) {
		return fmt.Errorf("--authority %q is not one of %s", *authority, strings.Join(offered, ", "))
	}
	body, err := signerCall(sg, http.MethodGet, "/v1/ca", nil)
	if err != nil {
		return err
	}
	var a identity.Authorities
	if err := json.Unmarshal(body, &a); err != nil {
		return fmt.Errorf("the signer's authorities: %w", err)
	}
	der := map[string][]byte{"orch": a.Orch, "operator": a.Operator, "host": a.Host}
	for _, n := range offered {
		if *authority != "" && *authority != n {
			continue
		}
		cert, err := x509.ParseCertificate(der[n])
		if err != nil {
			return fmt.Errorf("the signer's %s authority: %w", n, err)
		}
		sum := sha256.Sum256(cert.Raw)
		_, _ = fmt.Fprintf(stdout, "# %s authority, SHA-256 %s\n%s", n, hex.EncodeToString(sum[:]), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	}
	return nil
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
	pub, err := readJoinKey(*joinKey)
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

// x25519Size is the length of an X25519 public key.
const x25519Size = 32

// readJoinKey reads a join key as naru-signer join-key writes it, 32 raw bytes, or as base64 text of
// them, as it is when copied between hosts.
func readJoinKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == x25519Size {
		return b, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(decoded) != x25519Size {
		return nil, fmt.Errorf("%s is neither a 32-byte X25519 public key nor its base64 text", path)
	}
	return decoded, nil
}

func signerBundle(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("signer bundle", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the sealed bundle is written to (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sg.check(fs); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "out"); err != nil {
		return err
	}
	signed, err := signAs(*admin, *pass, identity.ActionBundle, struct{}{})
	if err != nil {
		return err
	}
	bundle, err := signerDo(sg, http.MethodGet, "/v1/bundle", nil, map[string]string{signerRequestHeader: base64.StdEncoding.EncodeToString(signed)})
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
	sg := addSignerFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sg.check(fs); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "bundle", "recovery-key", "recovery-passphrase-file"); err != nil {
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
	if _, err := signerCall(sg, http.MethodPost, "/v1/restore", body); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: the signer restored the authorities from %s\n", *bundleFile)
	return nil
}

// signerReissueRoot has the holder of the recovery key ask the signer for a new root certificate.
// The key is opened here and signs the request; it is never printed or written unencrypted.
func signerReissueRoot(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("signer reissue-root", stderr)
	recKey := fs.String("recovery-key", "", "the encrypted recovery key file")
	pass := fs.String("passphrase-file", "", "file holding the recovery key's passphrase")
	csrFile := fs.String("csr", "", "the certificate request for the new root key, DER or PEM")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the new root certificate is written to, PEM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sg.check(fs); err != nil {
		return err
	}
	if err := require(fs, "recovery-key", "passphrase-file", "csr", "out"); err != nil {
		return err
	}
	pw, err := passphrase(*pass)
	if err != nil {
		return err
	}
	keyFile, err := os.ReadFile(*recKey)
	if err != nil {
		return fmt.Errorf("read the recovery key: %w", err)
	}
	recovery, err := identity.DecryptRecovery(pw, keyFile)
	if err != nil {
		return fmt.Errorf("open %s: %w", *recKey, err)
	}
	csr, err := readCSR(*csrFile)
	if err != nil {
		return err
	}
	nonce, err := identity.NewNonce()
	if err != nil {
		return err
	}
	signed, err := identity.SignRecoveryRequest(identity.ActionReissueRoot, identity.ReissueRootRequest{Nonce: nonce, CSR: csr},
		time.Now().Add(operatorRequestTTL), recovery.Ed25519)
	if err != nil {
		return err
	}
	body, err := signerCall(sg, http.MethodPost, "/v1/reissue-root", signed)
	if err != nil {
		return err
	}
	var issued identity.Issued
	if err := json.Unmarshal(body, &issued); err != nil {
		return fmt.Errorf("the signer's answer: %w", err)
	}
	if _, err := x509.ParseCertificate(issued.Certificate); err != nil {
		return fmt.Errorf("the signer's certificate: %w", err)
	}
	if err := os.WriteFile(*out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate}), 0o644); err != nil { //nolint:gosec // a certificate holds nothing secret
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: the signer issued a new root certificate, serial %s, valid until %s, and revoked the old root; wrote %s\n",
		issued.Serial, issued.NotAfter.Format(time.RFC3339), *out)
	return nil
}
