package pki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	if b == nil {
		t.Fatal("not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssue_ALeafDoesNotOutliveTheAuthority(t *testing.T) {
	ca, err := New("test")
	if err != nil {
		t.Fatal(err)
	}
	// An authority with an hour left: a leaf of LeafValidity would outlive it.
	ca.cert.NotAfter = time.Now().Add(time.Hour)

	p, err := ca.Client("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := parse(t, p.CertPEM).NotAfter; got.After(ca.cert.NotAfter) {
		t.Errorf("leaf expires %v, after the authority's %v", got, ca.cert.NotAfter)
	}
}

func TestLoad_RefusesAKeyThatIsNotTheCertificates(t *testing.T) {
	a, err := New("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := load(a.CertPEM, b.KeyPEM); err == nil {
		t.Error("load accepted a key that is not the certificate's")
	}
}
