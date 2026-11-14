package command

import (
	"bytes"
	"context"
	"crypto/x509"
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

// operatorRequestTTL is how long a request naructl signs stays valid; a replay after it is refused.
const operatorRequestTTL = 2 * time.Minute

const operatorUsage = `usage: naructl operator add --admin <person file> --passphrase-file <file> --csr <file> --name <name> --role operator|reader|automation --socket <path> --out <cert file>
       naructl operator revoke --admin <person file> --passphrase-file <file> --serial <hex> --socket <path>

  The admin file is root's (to add) or an operator's (to revoke) encrypted person file; the signer's
  socket is the one naru-signer serves. The CSR is a DER or PEM PKCS #10 request the person made.
`

func operatorCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, operatorUsage)
		return errors.New("operator add or revoke is required")
	}
	switch args[0] {
	case "add":
		return operatorAdd(args[1:], stdout, stderr)
	case "revoke":
		return operatorRevoke(args[1:], stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, operatorUsage)
	return fmt.Errorf("operator %q: add or revoke", args[0])
}

func operatorAdd(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("operator add", stderr)
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	csrFile := fs.String("csr", "", "the new identity's certificate request, DER or PEM")
	name := fs.String("name", "", "the certificate's CN")
	role := fs.String("role", "operator", "operator, reader or automation")
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
	signed, err := signAs(*admin, *pass, identity.ActionIssueOperator, identity.IssueRequest{Name: *name, Role: identity.Role(*role), CSR: csr})
	if err != nil {
		return err
	}
	body, err := signerCall(*socket, http.MethodPost, "/v1/operator", signed)
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
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued.Certificate})
	if err := os.WriteFile(*out, pemBytes, 0o644); err != nil { //nolint:gosec // a certificate holds nothing secret
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: issued a %s certificate for %s, serial %s, valid until %s; wrote %s\n",
		*role, *name, issued.Serial, issued.NotAfter.Format(time.RFC3339), *out)
	return nil
}

func operatorRevoke(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("operator revoke", stderr)
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	serial := fs.String("serial", "", "the certificate's serial, hexadecimal")
	socket := fs.String("socket", "", "naru-signer's unix socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "admin", "passphrase-file", "serial", "socket"); err != nil {
		return err
	}
	signed, err := signAs(*admin, *pass, identity.ActionRevoke, identity.RevokeRequest{Serial: strings.ToLower(*serial)})
	if err != nil {
		return err
	}
	if _, err := signerCall(*socket, http.MethodPost, "/v1/revoke", signed); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: revoked serial %s\n", strings.ToLower(*serial))
	return nil
}

// signAs opens the person file and signs body for action with its key.
func signAs(adminFile, passphraseFile, action string, body any) ([]byte, error) {
	data, err := os.ReadFile(adminFile)
	if err != nil {
		return nil, err
	}
	pass, err := os.ReadFile(passphraseFile)
	if err != nil {
		return nil, err
	}
	person, err := identity.DecryptPerson(strings.TrimRight(string(pass), "\r\n"), data)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", adminFile, err)
	}
	return identity.SignRequest(action, body, time.Now().Add(operatorRequestTTL), person.Key, person.Cert)
}

func readCSR(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(data); block != nil {
		return block.Bytes, nil
	}
	return data, nil
}

// signerCall sends body to the signer on its unix socket and returns the answer's body, or the
// signer's refusal.
func signerCall(socket, method, path string, body []byte) ([]byte, error) {
	client := &http.Client{
		Timeout: callTimeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}
	req, err := http.NewRequest(method, "http://naru-signer"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the signer at %s: %w", socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the signer refused (%s): %s", resp.Status, strings.TrimSpace(string(answer)))
	}
	return answer, nil
}
