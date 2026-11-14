package identity

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"fmt"
)

const personBlock = "NARU PERSON FILE"

// Person is what a person or tool holds: a private key and the certificate
// an authority issued for it.
type Person struct {
	Key  crypto.Signer
	Cert *x509.Certificate
}

type personPlain struct {
	KeyDER  []byte `json:"keyDER"`
	CertDER []byte `json:"certDER"`
}

// EncryptPerson returns the person file: the key (PKCS #8) and certificate,
// encrypted under passphrase.
func EncryptPerson(passphrase string, p Person) ([]byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(p.Key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	plain, err := json.Marshal(personPlain{KeyDER: keyDER, CertDER: p.Cert.Raw})
	if err != nil {
		return nil, fmt.Errorf("marshal person: %w", err)
	}
	return sealFile(personBlock, passphrase, plain)
}

// DecryptPerson opens a person file.
func DecryptPerson(passphrase string, data []byte) (Person, error) {
	plain, err := openFile(personBlock, passphrase, data)
	if err != nil {
		return Person{}, err
	}
	var pp personPlain
	if err := json.Unmarshal(plain, &pp); err != nil {
		return Person{}, fmt.Errorf("parse person: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(pp.KeyDER)
	if err != nil {
		return Person{}, fmt.Errorf("parse key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return Person{}, fmt.Errorf("%w: key cannot sign", ErrFormat)
	}
	cert, err := x509.ParseCertificate(pp.CertDER)
	if err != nil {
		return Person{}, fmt.Errorf("parse certificate: %w", err)
	}
	return Person{Key: signer, Cert: cert}, nil
}
