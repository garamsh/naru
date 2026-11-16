package command

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
)

// signerRequestHeader carries a signed request on the GET that has no body.
const signerRequestHeader = "X-Naru-Request"

// signerCA is signer ca and orch ca: it reads GET /v1/ca from the signer, which answers without a
// signature, and prints the certificates of the authorities named in offered, in that order, as
// PEM each after a comment line that names it and gives the SHA-256 of its DER. --authority narrows
// the output to one of offered.
func signerCA(offered ...string) binder {
	return func(fs *pflag.FlagSet) runner {
		return signerCARunner(fs, offered)
	}
}

func signerCARunner(fs *pflag.FlagSet, offered []string) runner {
	authority := fs.String("authority", "", "print only this authority: "+strings.Join(offered, ", "))
	sg := addSignerFlags(fs)
	return func(_ []string, stdout, _ io.Writer) error {
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
}

func signerAuthoriseReplica(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host ID of the new signer")
	joinKey := fs.String("join-key", "", "the public half naru-signer join-key wrote on the new signer's host")
	out := fs.String("out", "", "the file the signed authorisation is written to")
	return func(args []string, stdout, stderr io.Writer) error {
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

func signerBundle(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the sealed bundle is written to (0600)")
	return func(args []string, stdout, stderr io.Writer) error {
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
}

func signerRestore(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	bundleFile := fs.String("bundle", "", "the sealed bundle signer bundle wrote")
	recKey := fs.String("recovery-key", "", "the recovery key file, as a path on the signer's host")
	recPass := fs.String("recovery-passphrase-file", "", "the file holding its passphrase, as a path on the signer's host")
	sg := addSignerFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
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
}

// signerReissueRoot has the holder of the recovery key ask the signer for a new root certificate.
// The key is opened here and signs the request; it is never printed or written unencrypted.
func signerReissueRoot(fs *pflag.FlagSet) runner {
	recKey := fs.String("recovery-key", "", "the encrypted recovery key file")
	pass := fs.String("passphrase-file", "", "file holding the recovery key's passphrase")
	csrFile := fs.String("csr", "", "the certificate request for the new root key, DER or PEM")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the new root certificate is written to, PEM")
	return func(args []string, stdout, stderr io.Writer) error {
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
		if err := os.WriteFile(*out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate}), 0o644); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: the signer issued a new root certificate, serial %s, valid until %s, and revoked the old root; wrote %s\n",
			issued.Serial, issued.NotAfter.Format(time.RFC3339), *out)
		return nil
	}
}

// signerRotateHost makes a new key for a host, has the signer issue a host certificate for it and
// put the serials it holds for the host on the revocation list, and writes the new pair. Nothing is
// written when the signer refuses.
func signerRotateHost(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host whose pair is replaced")
	keyOut := fs.String("key-out", "", "the file the new private key is written to (0600), PKCS #8 PEM")
	certOut := fs.String("cert-out", "", "the file the new certificate is written to, PEM")
	sg := addSignerFlags(fs)
	return func(_ []string, stdout, _ io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "host-id", "key-out", "cert-out"); err != nil {
			return err
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: *hostID}}, key)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, identity.ActionRotateHost, identity.RotateHostRequest{HostID: *hostID, CSR: csr})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, "/v1/rotate-host", signed)
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
		// The old serial is revoked by now: the key is written first and never over a file, so a
		// certificate nobody holds the key of is never the only thing left.
		if err := writeNew(*keyOut, pem.EncodeToMemory(&pem.Block{Type: keyBlock, Bytes: keyDER}), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate}), 0o644); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: rotated %s: the old serial is revoked; wrote the new key %s (0600) and certificate %s, serial %s, valid until %s\n",
			*hostID, *keyOut, *certOut, issued.Serial, issued.NotAfter.Format(time.RFC3339))
		return nil
	}
}

// signerRegisterHost records the serial of a certificate another authority issued to a host, such as
// the offline pair gen node writes, so that rotate-host can revoke it.
func signerRegisterHost(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host the certificate was issued to")
	certFile := fs.String("cert", "", "the host's certificate, PEM or DER, whose serial is registered")
	sg := addSignerFlags(fs)
	return func(_ []string, stdout, _ io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "host-id", "cert"); err != nil {
			return err
		}
		der, err := readCSR(*certFile)
		if err != nil {
			return err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("%s: %w", *certFile, err)
		}
		if cert.Subject.CommonName != *hostID {
			return fmt.Errorf("%s is the certificate of %q, not of %q", *certFile, cert.Subject.CommonName, *hostID)
		}
		serial := identity.SerialOf(cert)
		signed, err := signAs(*admin, *pass, identity.ActionRegisterHost, identity.RegisterHostRequest{HostID: *hostID, Serial: serial})
		if err != nil {
			return err
		}
		if _, err := signerCall(sg, http.MethodPost, "/v1/register-host", signed); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: registered serial %s for %s; rotate-host revokes it\n", serial, *hostID)
		return nil
	}
}
