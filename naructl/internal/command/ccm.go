package command

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
)

// ccmCredential makes the pair a cloud-controller-manager calls orch with. The key is made here and
// goes to no one but --key-out; the signer sees only its certificate request.
func ccmCredential(fs *pflag.FlagSet) runner {
	network := fs.String("network", "", "the network the controller serves; the certificate's CN is exposer.<network>")
	days := fs.Int("days", identity.MaxExposerDays, fmt.Sprintf("days the certificate is valid, at most %d", identity.MaxExposerDays))
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	sg := addSignerFlags(fs)
	certOut := fs.String("cert-out", "", "the file the issued certificate is written to, PEM")
	keyOut := fs.String("key-out", "", "the file the new private key is written to (0600), PKCS #8 PEM")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "network", "admin", "passphrase-file", "cert-out", "key-out"); err != nil {
			return err
		}
		subject, ok := identity.ExposerSubject(*network)
		if !ok {
			return fmt.Errorf("--network %q is not a network name: a lowercase DNS label", *network)
		}
		if *days < 1 || *days > identity.MaxExposerDays {
			return fmt.Errorf("--days is 1 to %d", identity.MaxExposerDays)
		}
		for _, out := range []string{*certOut, *keyOut} {
			if _, err := os.Lstat(out); err == nil {
				return fmt.Errorf("%s exists: a credential is never written over a file", out)
			}
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: subject}}, key)
		if err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, identity.ActionIssueExposer, identity.ExposerRequest{Network: *network, Days: *days, CSR: csr})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, "/v1/exposer", signed)
		if err != nil {
			return err
		}
		var issued identity.Issued
		if err := json.Unmarshal(body, &issued); err != nil {
			return fmt.Errorf("the signer's answer: %w", err)
		}
		cert, err := x509.ParseCertificate(issued.Certificate)
		if err != nil {
			return fmt.Errorf("the signer's certificate: %w", err)
		}
		if pub, isECDSA := cert.PublicKey.(*ecdsa.PublicKey); cert.Subject.CommonName != subject || !isECDSA || !pub.Equal(&key.PublicKey) {
			return fmt.Errorf("the signer's certificate is not %s for this key; nothing was written", subject)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := writeNew(*keyOut, pem.EncodeToMemory(&pem.Block{Type: keyBlock, Bytes: keyDER}), 0o600); err != nil {
			return err
		}
		if err := writeNew(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate}), 0o644); err != nil {
			return fmt.Errorf("the key %s is written, but the certificate (serial %s) is not: %w", *keyOut, issued.Serial, err)
		}
		_, _ = fmt.Fprintf(stdout, "naructl: issued an exposer certificate for %s, serial %s, valid until %s; wrote %s and the key %s (0600)\n",
			subject, issued.Serial, issued.NotAfter.Format(time.RFC3339), *certOut, *keyOut)
		return nil
	}
}
