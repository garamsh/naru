package command

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
)

// operatorRequestTTL is how long a request naructl signs stays valid; a replay after it is refused.
const operatorRequestTTL = 2 * time.Minute

func operatorAdd(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	csrFile := fs.String("csr", "", "the new identity's certificate request, DER or PEM")
	name := fs.String("name", "", "the certificate's CN")
	role := fs.String("role", "operator", "operator, reader or automation")
	sg := addSignerFlags(fs)
	out := fs.String("out", "", "the file the issued certificate is written to, PEM")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "csr", "name", "out"); err != nil {
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
		body, err := signerCall(sg, http.MethodPost, "/v1/operator", signed)
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
}

func operatorRevoke(fs *pflag.FlagSet) runner {
	admin := fs.String("admin", "", "root's or an operator's encrypted person file")
	pass := fs.String("passphrase-file", "", "file holding the person file's passphrase")
	serial := fs.String("serial", "", "the certificate's serial, hexadecimal")
	sg := addSignerFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := sg.check(fs); err != nil {
			return err
		}
		if err := require(fs, "admin", "passphrase-file", "serial"); err != nil {
			return err
		}
		signed, err := signAs(*admin, *pass, identity.ActionRevoke, identity.RevokeRequest{Serial: strings.ToLower(*serial)})
		if err != nil {
			return err
		}
		if _, err := signerCall(sg, http.MethodPost, "/v1/revoke", signed); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: revoked serial %s\n", strings.ToLower(*serial))
		return nil
	}
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
