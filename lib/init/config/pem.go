package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

func parseCertificate(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(s string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("is not PEM")
	}
	var parsed any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("has the unsupported PEM type %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("is not a signing key")
	}
	return signer, nil
}

func publicKeysEqual(a, b any) bool {
	switch a := a.(type) {
	case *ecdsa.PublicKey:
		b, ok := b.(*ecdsa.PublicKey)
		return ok && a.Equal(b)
	case *rsa.PublicKey:
		b, ok := b.(*rsa.PublicKey)
		return ok && a.Equal(b)
	case ed25519.PublicKey:
		b, ok := b.(ed25519.PublicKey)
		return ok && a.Equal(b)
	}
	return false
}
