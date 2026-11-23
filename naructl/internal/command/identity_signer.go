package command

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/naructl/internal/client"
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

// signerRotateHost rotates one host's pair in the order that never strands the host (ADR 0077): the
// signer issues a new certificate for a key made here, the host API installs the pair and answers
// once the host reports to orch with it, and only then does the signer revoke the host's older
// serials. A failure before the last step leaves the old pair valid and in use; the new serial stays
// with the signer and is revoked by the next rotation. The new key is sent to the host and written
// nowhere else.
func signerRotateHost(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host whose pair is replaced")
	hostClient := fs.String("host-client", "", "the host API client file written by gen host-client, of the provisioner role")
	endpoint := fs.String("host", "", "the host's address, an IP address with or without a port (default the client file's first endpoint)")
	serverName := fs.String("host-server-name", "", "the name to verify the host's certificate against instead of its address: the host ID h-<id> of a host that joined from the install seed")
	sg := addSignerFlags(fs)
	return func(_ []string, stdout, _ io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "host-id", "host-client"); err != nil {
			return err
		}
		hc, err := loadHostClient(*hostClient, *endpoint, *serverName)
		if err != nil {
			return err
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: *hostID}, IPAddresses: hc.addressIPs()}, key)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, identity.ActionReissueHost, identity.ReissueHostRequest{HostID: *hostID, CSR: csr})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, "/v1/reissue-host", signed)
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

		served, err := hc.install(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate}), pem.EncodeToMemory(&pem.Block{Type: keyBlock, Bytes: keyDER}))
		if err != nil {
			return fmt.Errorf("the host did not take the new pair, so its old pair is still valid and in use (the new serial %s is revoked by the next rotation): %w", issued.Serial, err)
		}
		if served != issued.Serial {
			return fmt.Errorf("the host reports with serial %s, not the new %s, so the old pair is not revoked", served, issued.Serial)
		}

		if err := hc.waitServing(issued.Serial); err != nil {
			return fmt.Errorf("the host reports with the new pair but its API does not present it, so the old pair is not revoked: %w", err)
		}

		signed, err = signAs(*admin, *pass, identity.ActionRetireHost, identity.RetireHostRequest{HostID: *hostID, Keep: issued.Serial})
		if err != nil {
			return err
		}
		if _, err := signerCall(sg, http.MethodPost, "/v1/retire-host", signed); err != nil {
			return fmt.Errorf("the host reports with the new pair, serial %s, but its old serials are not revoked yet; run rotate-host again: %w", issued.Serial, err)
		}
		_, _ = fmt.Fprintf(stdout, "naructl: rotated %s: it reports to orch with the new pair, serial %s, valid until %s, and its old serials are revoked\n",
			*hostID, issued.Serial, issued.NotAfter.Format(time.RFC3339))
		return nil
	}
}

// hostPair is a client of one host API for the one call a rotation makes. It is built here and not
// from the hostapi client, which has no call for the host's pair.
type hostPair struct {
	hc   *http.Client
	base string
	addr string
	conf *tls.Config
}

// servedSerial is the serial, in hexadecimal, of the certificate the host API presents on a new
// connection.
func (h hostPair) servedSerial() (string, error) {
	d := &net.Dialer{Timeout: callTimeout}
	c, err := tls.DialWithDialer(d, "tcp", h.addr, h.conf)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	return c.ConnectionState().PeerCertificates[0].SerialNumber.Text(16), nil
}

// waitServing waits for the host API to present the certificate of serial on a new connection: the
// old serials are revoked only when new clients get the new pair.
func (h hostPair) waitServing(serial string) error {
	var last error
	for deadline := time.Now().Add(servingWait); time.Now().Before(deadline); time.Sleep(time.Second) {
		got, err := h.servedSerial()
		if err == nil && got == serial {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("it presents serial %s", got)
		}
		last = err
	}
	return last
}

// servingWait is how long rotate-host waits for the host API to present the new certificate.
const servingWait = 30 * time.Second

func loadHostClient(file, endpoint, serverName string) (hostPair, error) {
	f, err := client.LoadFile(file)
	if err != nil {
		return hostPair{}, err
	}
	if endpoint == "" {
		endpoint = f.Endpoints[0]
	}
	addr, err := client.AddressPort(endpoint, wire.HostAPIPort)
	if err != nil {
		return hostPair{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.CA)) {
		return hostPair{}, errors.New("the client file's ca is not a PEM certificate")
	}
	pair, err := tls.X509KeyPair([]byte(f.Certificate), []byte(f.Key))
	if err != nil {
		return hostPair{}, fmt.Errorf("the client file's certificate and key: %w", err)
	}
	conf := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName, Certificates: []tls.Certificate{pair}}
	return hostPair{hc: &http.Client{Transport: &http.Transport{TLSClientConfig: conf}, Timeout: callTimeout}, base: "https://" + addr, addr: addr, conf: conf}, nil
}

// install is PUT /v1/identity of the host API: it returns the serial the host reports with once the
// call has answered.
func (h hostPair) install(certPEM, keyPEM []byte) (string, error) {
	body, err := json.Marshal(map[string]string{"certificate": string(certPEM), "key": string(keyPEM)})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPut, h.base+"/v1/identity", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("the host API at %s: %w", h.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(answer, &e)
		return "", fmt.Errorf("the host API answered %d: %s", resp.StatusCode, e.Error)
	}
	var out struct {
		Serial string `json:"serial"`
	}
	if err := json.Unmarshal(answer, &out); err != nil {
		return "", fmt.Errorf("the host API's answer: %w", err)
	}
	return out.Serial, nil
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

// signerLookupHost lists every serial the signer holds for a host, so that a lost host's certificate
// can be revoked with the host ID alone.
func signerLookupHost(fs *pflag.FlagSet) runner {
	return signerHostSerials(fs, identity.ActionLookupHost, "/v1/lookup-host", func(stdout io.Writer, got identity.HostSerials) {
		_, _ = fmt.Fprintf(stdout, "naructl: the signer holds %d serial(s) for %s; naructl signer revoke-host revokes every one, naructl operator revoke --serial one\n", len(got.Serials), got.HostID)
		printHostSerials(stdout, got)
	})
}

// signerRevokeHost has the signer revoke every serial it holds for a host.
func signerRevokeHost(fs *pflag.FlagSet) runner {
	return signerHostSerials(fs, identity.ActionRevokeHost, "/v1/revoke-host", func(stdout io.Writer, got identity.HostSerials) {
		_, _ = fmt.Fprintf(stdout, "naructl: revoked every serial the signer holds for %s\n", got.HostID)
		printHostSerials(stdout, got)
	})
}

// signerHostSerials is the command that signs action for --host-id, posts it to path and prints the
// answer with report.
func signerHostSerials(fs *pflag.FlagSet, action, path string, report func(io.Writer, identity.HostSerials)) runner {
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	hostID := fs.String("host-id", "", "the host whose serials the signer holds")
	sg := addSignerFlags(fs)
	return func(_ []string, stdout, _ io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "host-id"); err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, action, identity.HostSerialsRequest{HostID: *hostID})
		if err != nil {
			return err
		}
		body, err := signerCall(sg, http.MethodPost, path, signed)
		if err != nil {
			return err
		}
		var got identity.HostSerials
		if err := json.Unmarshal(body, &got); err != nil {
			return fmt.Errorf("the signer's answer: %w", err)
		}
		report(stdout, got)
		return nil
	}
}

func printHostSerials(stdout io.Writer, got identity.HostSerials) {
	for _, h := range got.Serials {
		state := "valid"
		if h.Revoked {
			state = "revoked"
		}
		_, _ = fmt.Fprintf(stdout, "%s %s\n", h.Serial, state)
	}
}

// addressIPs is the host API's address when it is an IP, which the new serving certificate has to
// name for the clients that connect by it.
func (h hostPair) addressIPs() []net.IP {
	host, _, err := net.SplitHostPort(h.addr)
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	return nil
}
