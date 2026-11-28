package identity

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"time"
)

const (
	// AuthorityYears is how long an authority's self-signed certificate is valid (ADR 0121).
	AuthorityYears = 10
	// AuthorityRenewYears is what remains of an authority's certificate when the signer signs it
	// again under the same key: every pool that trusts the authority takes the new certificate
	// within these years, or stops trusting it when the old one expires (ADR 0121).
	AuthorityRenewYears = 2
	// AuthorityWarnYears is what remains of the last certificate a pool holds for an authority when
	// its holder warns: a pool that missed the refresh is seen a year before it breaks (ADR 0121).
	AuthorityWarnYears = 1
)

// RenewsBy is when the signer signs cert again: AuthorityRenewYears before it expires.
func RenewsBy(cert *x509.Certificate) time.Time {
	return cert.NotAfter.AddDate(-AuthorityRenewYears, 0, 0)
}

// TrustEnd is when a pool stops trusting an authority it holds, and that authority's subject: its
// common name, or the whole subject when it has none.
type TrustEnd struct {
	At        time.Time
	Authority string
}

// TrustExpiry reads a pool's PEM and returns when it stops trusting some authority it holds: for
// each authority, by subject and key, the latest NotAfter among its certificates, and of those the
// earliest, with that authority's subject. A certificate that is not a CA is no authority and is
// skipped. ok is false for a pool that holds no authority.
func TrustExpiry(poolPEM []byte) (end TrustEnd, ok bool) {
	type authority struct{ subject, key string }
	latest := map[authority]TrustEnd{}
	for rest := poolPEM; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			continue
		}
		k := authority{string(cert.RawSubject), string(cert.RawSubjectPublicKeyInfo)}
		if cert.NotAfter.After(latest[k].At) {
			name := cert.Subject.CommonName
			if name == "" {
				name = cert.Subject.String()
			}
			latest[k] = TrustEnd{At: cert.NotAfter, Authority: name}
		}
	}
	for _, t := range latest {
		if !ok || t.At.Before(end.At) || t.At.Equal(end.At) && t.Authority < end.Authority {
			end, ok = t, true
		}
	}
	return end, ok
}

// TrustWarns reports whether a pool whose trust expires at expiry is to be warned of at now:
// within AuthorityWarnYears of it, or past it.
func TrustWarns(expiry, now time.Time) bool {
	return !now.Before(expiry.AddDate(-AuthorityWarnYears, 0, 0))
}

// resigningSkew bounds how far before its signing a re-signing may start: the signer starts one a
// minute early, and a day allows for any signer's clock.
const resigningSkew = 24 * time.Hour

// Resigns reports whether c is a re-signing of an authority in trusted (ADR 0122 item 4): an
// authority's certificate, self-issued and signed by its own key, of the subject and key of one in
// trusted, living at most AuthorityYears and a day, and ending after every certificate of that
// subject and key there. A certificate of the authority's name and key that another key signed is
// a forged root, and is not one.
func Resigns(c *x509.Certificate, trusted []*x509.Certificate) bool {
	if !c.IsCA {
		return false
	}
	if !bytes.Equal(c.RawIssuer, c.RawSubject) || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil {
		return false
	}
	if c.NotAfter.After(c.NotBefore.AddDate(AuthorityYears, 0, 0).Add(resigningSkew)) {
		return false
	}
	known := false
	for _, t := range trusted {
		if !bytes.Equal(t.RawSubject, c.RawSubject) || !bytes.Equal(t.RawSubjectPublicKeyInfo, c.RawSubjectPublicKeyInfo) {
			continue
		}
		if !c.NotAfter.After(t.NotAfter) {
			return false
		}
		known = true
	}
	return known
}

// Resignings returns, of the DER certificates offered, each that Resigns of the certificates in
// trustedPEM and of those taken before it, as PEM in the order offered, and each one's certificate.
// What cannot be parsed is not one.
func Resignings(trustedPEM []byte, offered ...[]byte) ([]byte, []*x509.Certificate) {
	trusted := Certificates(trustedPEM)
	var out []byte
	var took []*x509.Certificate
	for _, der := range offered {
		c, err := x509.ParseCertificate(der)
		if err != nil || !Resigns(c, trusted) {
			continue
		}
		trusted, took = append(trusted, c), append(took, c)
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out, took
}

// Certificates parses every CERTIFICATE block in b, skipping what is not one.
func Certificates(b []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return out
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil && block.Type == "CERTIFICATE" {
			out = append(out, c)
		}
	}
}
