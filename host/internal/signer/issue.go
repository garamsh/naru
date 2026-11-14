package signer

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// certificateYears is how long an operator, reader, automation, orch or host certificate is valid.
const certificateYears = 1

// namePattern is what a certificate's CN may be: a host ID or a person's or tool's name.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)

// InitOptions is what Init needs besides the signer's own directory.
type InitOptions struct {
	// RecoveryPublic is the public half of the recovery key, which the bundle is sealed to.
	RecoveryPublic identity.RecoveryPublic
	// RootCSR is the certificate request of the first root identity, DER.
	RootCSR []byte
}

// Init makes every authority in a blank directory and issues the first root certificate for the
// request in opts. The keys never leave the directory.
func (s *Signer) Init(opts InitOptions) (identity.Issued, error) {
	csr, err := parseCSR(opts.RootCSR)
	if err != nil {
		return identity.Issued{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth != nil {
		return identity.Issued{}, ErrInitialised
	}
	now := s.now()
	a, err := newAuthorities(opts.RecoveryPublic, now)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := a.check(); err != nil {
		return identity.Issued{}, err
	}
	root, err := issueLeaf(a.operator, now, "root", identity.RoleRoot, csr.PublicKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return identity.Issued{}, err
	}
	a.issued = map[string]string{root.Serial: string(identity.RoleRoot)}
	if err := a.save(s.dir); err != nil {
		return identity.Issued{}, err
	}
	s.auth = a
	s.log.Info("initialised the authorities", "root_serial", root.Serial)
	return root, nil
}

// IssueHost issues a host certificate for an admission statement an operator signed and the
// certificate request of the key the host made. The signer checks the statement itself: orch's
// word is not taken.
func (s *Signer) IssueHost(req identity.HostRequest) (identity.Issued, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Issued{}, err
	}
	now := s.now()
	operator, adm, err := identity.VerifyAdmission(req.Statement, a.operatorPool(), &a.crl, now)
	if err != nil {
		return identity.Issued{}, err
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	sum := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	if hex.EncodeToString(sum[:]) != strings.ToLower(adm.HostKeySHA256) {
		return identity.Issued{}, fmt.Errorf("%w: the request's key is not the one the statement names", ErrBadRequest)
	}
	if !namePattern.MatchString(adm.HostID) {
		return identity.Issued{}, fmt.Errorf("%w: host ID %q", ErrBadRequest, adm.HostID)
	}
	issued, err := issueLeaf(a.host, now, adm.HostID, identity.RoleHost, csr.PublicKey, serverAndClient, nil)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := s.record(issued.Serial, identity.RoleHost); err != nil {
		return identity.Issued{}, err
	}
	s.log.Info("issued a host certificate", "host_id", adm.HostID, "serial", issued.Serial, "key_sha256", adm.HostKeySHA256,
		"region", adm.Region, "authorised_by", operator.Subject, "authorised_by_serial", operator.Serial, "authorised_role", string(operator.Role))
	return issued, nil
}

// IssueOrch issues an orch certificate for a request root signed.
func (s *Signer) IssueOrch(signed []byte) (identity.Issued, error) {
	var req identity.IssueRequest
	who, a, err := s.authorise(signed, identity.ActionIssueOrch, &req, identity.RoleRoot)
	if err != nil {
		return identity.Issued{}, err
	}
	return s.issueFor(a, who, a.orch, req, identity.RoleOrch, serverAndClient)
}

// IssueOperator issues an operator, reader or automation certificate for a request root signed.
func (s *Signer) IssueOperator(signed []byte) (identity.Issued, error) {
	var req identity.IssueRequest
	who, a, err := s.authorise(signed, identity.ActionIssueOperator, &req, identity.RoleRoot)
	if err != nil {
		return identity.Issued{}, err
	}
	switch req.Role {
	case identity.RoleOperator, identity.RoleReader, identity.RoleAutomation:
	default:
		return identity.Issued{}, fmt.Errorf("%w: role %q is not one an operator certificate takes", ErrBadRequest, req.Role)
	}
	return s.issueFor(a, who, a.operator, req, req.Role, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

func (s *Signer) issueFor(a *authorities, who identity.Identity, ca authority, req identity.IssueRequest, role identity.Role, usages []x509.ExtKeyUsage) (identity.Issued, error) {
	if !namePattern.MatchString(req.Name) {
		return identity.Issued{}, fmt.Errorf("%w: name %q", ErrBadRequest, req.Name)
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	var names *x509.CertificateRequest
	if role == identity.RoleOrch {
		names = csr // orch serves its API under the names it asked for; root authorised them by signing the request
	}
	issued, err := issueLeaf(ca, s.now(), req.Name, role, csr.PublicKey, usages, names)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := s.record(issued.Serial, role); err != nil {
		return identity.Issued{}, err
	}
	s.log.Info("issued a certificate", "role", string(role), "subject", req.Name, "serial", issued.Serial,
		"authorised_by", who.Subject, "authorised_by_serial", who.Serial)
	return issued, nil
}

// record notes the role of a serial the signer issued, in its directory, before the certificate
// is handed out.
func (s *Signer) record(serial string, role identity.Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.auth.withIssued(serial, role)
	if err := writeFile(filepath.Join(s.dir, issuedFile), next.marshalIssued()); err != nil {
		return err
	}
	s.auth = next
	return nil
}

// authorise verifies a request signed for action by one of the roles allowed, decodes its body
// into body and returns who signed it and the authorities it was checked against.
func (s *Signer) authorise(signed []byte, action string, body any, allowed ...identity.Role) (identity.Identity, *authorities, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Identity{}, nil, err
	}
	who, raw, err := identity.VerifyRequest(signed, action, a.operatorPool(), &a.crl, s.now())
	if err != nil {
		return identity.Identity{}, nil, err
	}
	if !roleAllowed(who.Role, allowed) {
		return identity.Identity{}, nil, fmt.Errorf("%w: %s may not %s", ErrForbidden, who.Role, action)
	}
	if err := decode(raw, body); err != nil {
		return identity.Identity{}, nil, err
	}
	return who, a, nil
}

// decode reads a verified request's body into v.
func decode(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	return nil
}

func roleAllowed(role identity.Role, allowed []identity.Role) bool {
	for _, r := range allowed {
		if r == role {
			return true
		}
	}
	return false
}

var serverAndClient = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}

// issueLeaf signs a certificate of role for pub under ca, valid for certificateYears. A non-nil
// names gives the certificate its DNS names and IP addresses.
func issueLeaf(ca authority, now time.Time, cn string, role identity.Role, pub any, usages []x509.ExtKeyUsage, names *x509.CertificateRequest) (identity.Issued, error) {
	serial, err := randomSerial()
	if err != nil {
		return identity.Issued{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{string(role)}},
		NotBefore:    now.Add(-notBeforeBackoff),
		NotAfter:     now.AddDate(certificateYears, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	if names != nil {
		tmpl.DNSNames, tmpl.IPAddresses = names.DNSNames, names.IPAddresses
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return identity.Issued{}, fmt.Errorf("issue %s certificate: %w", role, err)
	}
	return identity.Issued{Certificate: der, Serial: serial.Text(16), NotAfter: tmpl.NotAfter}, nil
}

// parseCSR parses a certificate request, checks its own signature and that its key is ECDSA or Ed25519.
func parseCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("%w: certificate request: %w", ErrBadRequest, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: certificate request: %w", ErrBadRequest, err)
	}
	switch csr.PublicKey.(type) {
	case *ecdsa.PublicKey, ed25519.PublicKey:
		return csr, nil
	}
	return nil, fmt.Errorf("%w: certificate request key is neither ECDSA nor Ed25519", ErrBadRequest)
}
