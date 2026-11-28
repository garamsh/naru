package identity

import (
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

// TrustExpiry reads a pool's PEM and returns when it stops trusting some authority it holds: for
// each authority, by subject and key, the latest NotAfter among its certificates, and of those the
// earliest. A certificate that is not a CA is no authority and is skipped. ok is false for a pool
// that holds no authority.
func TrustExpiry(poolPEM []byte) (expiry time.Time, ok bool) {
	type authority struct{ subject, key string }
	latest := map[authority]time.Time{}
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
		if cert.NotAfter.After(latest[k]) {
			latest[k] = cert.NotAfter
		}
	}
	for _, t := range latest {
		if !ok || t.Before(expiry) {
			expiry, ok = t, true
		}
	}
	return expiry, ok
}

// TrustWarns reports whether a pool whose trust expires at expiry is to be warned of at now:
// within AuthorityWarnYears of it, or past it.
func TrustWarns(expiry, now time.Time) bool {
	return !now.Before(expiry.AddDate(-AuthorityWarnYears, 0, 0))
}
