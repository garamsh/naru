package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"math/big"
	"time"
)

// The keys of the test registry's authority and leaf (registrypki.go) are derived from the
// release's version, so that two builds of one commit write the same files (ADR 0078). The
// derivation is no secret: anyone who knows the version can compute them, and the registry is a
// test one.
const (
	testCAValidity = 10 * 365 * 24 * time.Hour
	testCASkew     = time.Hour
)

// derive is 32 bytes fixed by the version and the label.
func derive(version, label string) []byte {
	m := hmac.New(sha256.New, []byte("naru test user-data v1 "+version))
	m.Write([]byte(label))
	return m.Sum(nil)
}

// derivedKeyOf is the P-256 key the version and the label fix.
func derivedKeyOf(version, label string) (*ecdsa.PrivateKey, error) {
	n := elliptic.P256().Params().N
	d := new(big.Int).SetBytes(derive(version, label))
	d.Mod(d, new(big.Int).Sub(n, big.NewInt(1))).Add(d, big.NewInt(1))
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), d.FillBytes(make([]byte, 32)))
}
