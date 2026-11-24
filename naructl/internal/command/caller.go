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

// callerCredential makes the pair a use of the Machine API calls orch with, issued by root directly:
// for tests and recovery, since a caller registers with a token and its own key (ADR 0116). The key
// is made here and goes to no one but --key-out; the signer sees only its certificate request.
func callerCredential(fs *pflag.FlagSet) runner {
	name := fs.String("name", "", "the caller's name, a lowercase DNS label; the certificate's CN")
	days := fs.Int("days", identity.MaxCallerDays, fmt.Sprintf("days the certificate is valid, at most %d", identity.MaxCallerDays))
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	sg := addSignerFlags(fs)
	certOut := fs.String("cert-out", "", "the file the issued certificate is written to, PEM")
	keyOut := fs.String("key-out", "", "the file the new private key is written to (0600), PKCS #8 PEM")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "name", "admin", "passphrase-file", "cert-out", "key-out"); err != nil {
			return err
		}
		if !identity.IsCallerName(*name) {
			return fmt.Errorf("--name %q is not a caller name: a lowercase DNS label", *name)
		}
		if *days < 1 || *days > identity.MaxCallerDays {
			return fmt.Errorf("--days is 1 to %d", identity.MaxCallerDays)
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
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: *name}}, key)
		if err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, identity.ActionIssueCaller, identity.CallerRequest{Name: *name, Days: *days, CSR: csr})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, "/v1/caller-issue", signed)
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
		if pub, isECDSA := cert.PublicKey.(*ecdsa.PublicKey); cert.Subject.CommonName != *name || !isECDSA || !pub.Equal(&key.PublicKey) {
			return fmt.Errorf("the signer's certificate is not %s for this key; nothing was written", *name)
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
		_, _ = fmt.Fprintf(stdout, "naructl: issued a caller certificate for %s, serial %s, valid until %s; wrote %s and the key %s (0600)\n",
			*name, issued.Serial, issued.NotAfter.Format(time.RFC3339), *certOut, *keyOut)
		return nil
	}
}

// callerToken writes a registration token for a caller, which root asks the signer for (ADR 0116).
// The token is a secret until it is spent or expires: it goes only to --out, 0600.
func callerToken(fs *pflag.FlagSet) runner {
	name := fs.String("name", "", "the caller the token registers, a lowercase DNS label")
	ttl := fs.Duration("ttl", identity.DefaultCallerTokenTTL, fmt.Sprintf("how long the token lives, at most %s", identity.MaxCallerTokenTTL))
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the token is written to (0600)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "name", "admin", "passphrase-file", "out"); err != nil {
			return err
		}
		if !identity.IsCallerName(*name) {
			return fmt.Errorf("--name %q is not a caller name: a lowercase DNS label", *name)
		}
		if *ttl < time.Second || *ttl > identity.MaxCallerTokenTTL {
			return fmt.Errorf("--ttl is 1s to %s", identity.MaxCallerTokenTTL)
		}
		if _, err := os.Lstat(*out); err == nil {
			return fmt.Errorf("%s exists: a token is never written over a file", *out)
		}
		signed, err := signAs(*admin, *pass, identity.ActionIssueCallerToken, identity.CallerTokenRequest{Name: *name, TTLSeconds: int(*ttl / time.Second)})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, "/v1/caller-token", signed)
		if err != nil {
			return err
		}
		var tok identity.CallerToken
		if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
			return fmt.Errorf("the signer's answer holds no token")
		}
		if err := writeNew(*out, []byte(tok.Token+"\n"), 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: wrote a registration token for the caller %s to %s (0600); it is taken once, until %s\n",
			*name, *out, tok.ExpiresAt.Format(time.RFC3339))
		return nil
	}
}
