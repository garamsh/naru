package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// Algorithm is the JWS algorithm a ServiceAccount key signs tokens with (ADR 0087). The API server
// takes it from the key's type.
type Algorithm string

const (
	// RS256 is RSA 2048 with SHA-256, the algorithm OpenID Connect requires every provider to support.
	RS256 Algorithm = "RS256"
	// ES256 is ECDSA P-256 with SHA-256.
	ES256 Algorithm = "ES256"
)

// DefaultAlgorithm is the algorithm of a key made when the operator names none.
const DefaultAlgorithm = RS256

const rsaBits = 2048

// ParseAlgorithm returns the algorithm s names, and an error for any other.
func ParseAlgorithm(s string) (Algorithm, error) {
	switch a := Algorithm(s); a {
	case RS256, ES256:
		return a, nil
	}
	return "", fmt.Errorf("the algorithm %q is not RS256 or ES256", s)
}

// ServiceAccountKey returns a new key for signing ServiceAccount tokens with alg, as the PEM of the
// private key and of the public key.
func ServiceAccountKey(alg Algorithm) (keyPEM, pubPEM []byte, err error) {
	var key crypto.Signer
	var keyBlock *pem.Block
	switch alg {
	case RS256:
		k, err := rsa.GenerateKey(rand.Reader, rsaBits)
		if err != nil {
			return nil, nil, err
		}
		key, keyBlock = k, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	case ES256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, nil, err
		}
		key, keyBlock = k, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	default:
		return nil, nil, fmt.Errorf("the algorithm %q is not RS256 or ES256", alg)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(keyBlock), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}

// PublicKeys parses the PEM public keys of a ServiceAccount public key file, in order: the first is
// the signing key's, and any after it are keys whose tokens are still accepted.
func PublicKeys(pubPEM []byte) ([]crypto.PublicKey, error) {
	var keys []crypto.PublicKey
	for rest := pubPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		if _, err := algorithmOf(pub); err != nil {
			return nil, err
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, errors.New("no public key")
	}
	return keys, nil
}

func algorithmOf(pub crypto.PublicKey) (Algorithm, error) {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < rsaBits {
			return "", fmt.Errorf("an RSA key of %d bits is smaller than %d", k.N.BitLen(), rsaBits)
		}
		return RS256, nil
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() {
			return ES256, nil
		}
	}
	return "", errors.New("a ServiceAccount key is RSA or ECDSA P-256")
}

// KeyAlgorithm returns the algorithm the first public key of pubPEM, the signing key's, signs with.
func KeyAlgorithm(pubPEM []byte) (Algorithm, error) {
	keys, err := PublicKeys(pubPEM)
	if err != nil {
		return "", err
	}
	return algorithmOf(keys[0])
}

// RotatedPublicKeys returns the public key file after a rotation: newPub first, and the signing key of
// oldPub after it, so that tokens the old key signed stay valid. Any key older than that is dropped.
func RotatedPublicKeys(newPub, oldPub []byte) ([]byte, error) {
	if _, err := PublicKeys(newPub); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(oldPub)
	if block == nil {
		return nil, errors.New("the old public key is not PEM")
	}
	return append(append([]byte{}, newPub...), pem.EncodeToMemory(block)...), nil
}

// SigningPublicKey returns the PEM public key of the private key keyPEM, a ServiceAccount signing key
// as ServiceAccountKey writes it, and refuses a key of another type or a weak one.
func SigningPublicKey(keyPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the private key is not PEM")
	}
	var key crypto.Signer
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("a ServiceAccount key is an RSA or EC private key, not %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	if _, err := algorithmOf(key.Public()); err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// SigningKeyID returns the KeyID of the first public key of pubPEM, the signing key's.
func SigningKeyID(pubPEM []byte) (string, error) {
	keys, err := PublicKeys(pubPEM)
	if err != nil {
		return "", err
	}
	return KeyID(keys[0])
}

// KeyID is the "kid" the API server puts in the header of a token the key signs: the unpadded
// base64url of the SHA-256 of the key's PKIX encoding.
func KeyID(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// JWK is a public key as RFC 7517 writes it.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// ServiceAccountJWKS returns the JSON Web Key Set of the public keys in pubPEM, which holds no
// private value.
func ServiceAccountJWKS(pubPEM []byte) ([]byte, error) {
	keys, err := PublicKeys(pubPEM)
	if err != nil {
		return nil, err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	set := struct {
		Keys []JWK `json:"keys"`
	}{}
	for _, pub := range keys {
		alg, err := algorithmOf(pub)
		if err != nil {
			return nil, err
		}
		kid, err := KeyID(pub)
		if err != nil {
			return nil, err
		}
		jwk := JWK{Kid: kid, Use: "sig", Alg: string(alg)}
		switch k := pub.(type) {
		case *rsa.PublicKey:
			jwk.Kty, jwk.N, jwk.E = "RSA", b64(k.N.Bytes()), b64(big.NewInt(int64(k.E)).Bytes())
		case *ecdsa.PublicKey:
			// P-256 coordinates are 32 bytes, left-padded (RFC 7518 section 6.2.1.2).
			jwk.Kty, jwk.Crv = "EC", "P-256"
			jwk.X, jwk.Y = b64(k.X.FillBytes(make([]byte, 32))), b64(k.Y.FillBytes(make([]byte, 32)))
		}
		set.Keys = append(set.Keys, jwk)
	}
	return json.MarshalIndent(set, "", "  ")
}

// OIDCDiscovery returns the OpenID Connect discovery document of issuer, which names jwksURI and
// the algorithms of pubPEM's keys.
func OIDCDiscovery(issuer, jwksURI string, pubPEM []byte) ([]byte, error) {
	keys, err := PublicKeys(pubPEM)
	if err != nil {
		return nil, err
	}
	var algs []Algorithm
	for _, pub := range keys {
		alg, err := algorithmOf(pub)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(algs, alg) {
			algs = append(algs, alg)
		}
	}
	return json.MarshalIndent(map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              jwksURI,
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": algs,
	}, "", "  ")
}
