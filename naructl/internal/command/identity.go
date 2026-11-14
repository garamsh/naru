package command

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

const keyBlock = "PRIVATE KEY"

// operatorRequest writes a new ECDSA key and the certificate request for it, so the person or tool
// the key belongs to never has it leave this machine. The certificate comes from `operator add`
// (or `orch issue`, or naru-signer init for the first root) and `operator person` joins the two.
func operatorRequest(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("operator request", stderr)
	name := fs.String("name", "", "the certificate request's CN")
	keyOut := fs.String("key-out", "", "the file the new private key is written to (0600), PKCS #8 PEM")
	csrOut := fs.String("csr-out", "", "the file the certificate request is written to")
	der := fs.Bool("der", false, "write the request as DER, which naru-signer init reads, and not as PEM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "name", "key-out", "csr-out"); err != nil {
		return err
	}
	return writeRequest(*name, *keyOut, *csrOut, *der, nil, nil, stdout)
}

// orchCSR writes a new key and the certificate request of an orch (or any server) that serves under
// DNS names and IP addresses: the signer copies the names of an orch request into its certificate.
func orchCSR(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("orch csr", stderr)
	name := fs.String("name", "", "the certificate request's CN, orch's member name")
	dns := fs.String("dns", "", "comma-separated DNS names the certificate serves")
	ips := fs.String("ip", "", "comma-separated IP addresses the certificate serves")
	keyOut := fs.String("key-out", "", "the file the new private key is written to (0600), PKCS #8 PEM")
	csrOut := fs.String("csr-out", "", "the file the certificate request is written to")
	der := fs.Bool("der", false, "write the request as DER and not as PEM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "name", "key-out", "csr-out"); err != nil {
		return err
	}
	names := splitList(*dns)
	var addrs []net.IP
	for _, s := range splitList(*ips) {
		ip := net.ParseIP(s)
		if ip == nil {
			return fmt.Errorf("--ip: %q is not an IP address", s)
		}
		addrs = append(addrs, ip)
	}
	if len(names)+len(addrs) == 0 {
		return errors.New("--dns or --ip names what the certificate serves")
	}
	return writeRequest(*name, *keyOut, *csrOut, *der, names, addrs, stdout)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeRequest(name, keyOut, csrOut string, der bool, dns []string, ips []net.IP, stdout io.Writer) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name}, DNSNames: dns, IPAddresses: ips,
	}, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key is written first and never over a file: a request nobody holds the key of is worthless.
	if err := writeNew(keyOut, pem.EncodeToMemory(&pem.Block{Type: keyBlock, Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	out := csr
	if !der {
		out = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})
	}
	if err := os.WriteFile(csrOut, out, 0o644); err != nil { //nolint:gosec // a certificate request holds nothing secret
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote the key %s (0600) and the certificate request %s for %s\n", keyOut, csrOut, name)
	return nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // the caller names the file
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// operatorPerson joins a key `operator request` wrote with the certificate issued for it into the
// encrypted person file, and removes the plain key.
func operatorPerson(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("operator person", stderr)
	keyFile := fs.String("key", "", "the key operator request wrote; it is removed once the person file is written")
	certFile := fs.String("cert", "", "the certificate issued for that key, PEM")
	pass := fs.String("passphrase-file", "", "file holding the passphrase the person file is encrypted under")
	out := fs.String("out", "", "the person file to write (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "key", "cert", "passphrase-file", "out"); err != nil {
		return err
	}
	pw, err := passphrase(*pass)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	certPEM, err := os.ReadFile(*certFile)
	if err != nil {
		return err
	}
	kb, cb := firstBlock(keyPEM), firstBlock(certPEM)
	if kb == nil || kb.Type != keyBlock {
		return fmt.Errorf("%s holds no PKCS #8 private key", *keyFile)
	}
	if cb == nil || cb.Type != "CERTIFICATE" {
		return fmt.Errorf("%s holds no certificate", *certFile)
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	signer, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("the key is not an ECDSA key")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&signer.PublicKey) {
		return errors.New("the certificate is not for this key")
	}
	enc, err := identity.EncryptPerson(pw, identity.Person{Key: signer, Cert: cert})
	if err != nil {
		return err
	}
	if err := writeNew(*out, enc, 0o600); err != nil {
		return err
	}
	if err := os.Remove(*keyFile); err != nil {
		return fmt.Errorf("the person file is written, but the plain key stays: %w", err)
	}
	role, _ := identity.RoleOf(cert)
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s for %s (%s, serial %s) and removed the plain key\n", *out, cert.Subject.CommonName, role, identity.SerialOf(cert))
	return nil
}

func firstBlock(data []byte) *pem.Block {
	b, _ := pem.Decode(data)
	return b
}

// orchIssue has the signer issue orch's certificate for a request, with root's person file.
func orchIssue(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("orch issue", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	csrFile := fs.String("csr", "", "the certificate request orch csr wrote, DER or PEM")
	name := fs.String("name", "", "the certificate's CN")
	socket := fs.String("socket", "", "naru-signer's unix socket")
	out := fs.String("out", "", "the file the issued certificate is written to, PEM")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "csr", "name", "socket", "out"); err != nil {
		return err
	}
	csr, err := readCSR(*csrFile)
	if err != nil {
		return err
	}
	signed, err := signAs(*admin, *pass, identity.ActionIssueOrch, identity.IssueRequest{Name: *name, CSR: csr})
	if err != nil {
		return err
	}
	body, err := signerCall(*socket, http.MethodPost, "/v1/orch", signed)
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
	_, _ = fmt.Fprintf(stdout, "naructl: issued an orch certificate for %s, serial %s, valid until %s; wrote %s\n",
		*name, issued.Serial, issued.NotAfter.Format(time.RFC3339), *out)
	return nil
}
