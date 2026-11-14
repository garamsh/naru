package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
)

const recoveryBlock = "NARU RECOVERY KEY"

// Recovery is the recovery key: an X25519 key that the authorities' backup is
// sealed to, and an Ed25519 key that signs.
type Recovery struct {
	X25519  *ecdh.PrivateKey
	Ed25519 ed25519.PrivateKey
}

// RecoveryPublic is the public half of a Recovery, which may be kept anywhere.
type RecoveryPublic struct {
	X25519  []byte `json:"x25519"`
	Ed25519 []byte `json:"ed25519"`
}

type recoveryPlain struct {
	X25519Seed  []byte `json:"x25519"`
	Ed25519Seed []byte `json:"ed25519"`
}

// GenerateRecovery makes a new recovery key.
func GenerateRecovery() (Recovery, error) {
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Recovery{}, fmt.Errorf("generate x25519 key: %w", err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Recovery{}, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return Recovery{X25519: x, Ed25519: ed}, nil
}

// Public returns the public halves.
func (r Recovery) Public() RecoveryPublic {
	return RecoveryPublic{
		X25519:  r.X25519.PublicKey().Bytes(),
		Ed25519: r.Ed25519.Public().(ed25519.PublicKey),
	}
}

// Encrypt returns the recovery file: both private keys under passphrase.
func (r Recovery) Encrypt(passphrase string) ([]byte, error) {
	plain, err := json.Marshal(recoveryPlain{X25519Seed: r.X25519.Bytes(), Ed25519Seed: r.Ed25519.Seed()})
	if err != nil {
		return nil, fmt.Errorf("marshal recovery key: %w", err)
	}
	return sealFile(recoveryBlock, passphrase, plain)
}

// DecryptRecovery opens a recovery file.
func DecryptRecovery(passphrase string, data []byte) (Recovery, error) {
	plain, err := openFile(recoveryBlock, passphrase, data)
	if err != nil {
		return Recovery{}, err
	}
	var rp recoveryPlain
	if err := json.Unmarshal(plain, &rp); err != nil {
		return Recovery{}, fmt.Errorf("parse recovery key: %w", err)
	}
	x, err := ecdh.X25519().NewPrivateKey(rp.X25519Seed)
	if err != nil {
		return Recovery{}, fmt.Errorf("%w: x25519 key", ErrFormat)
	}
	if len(rp.Ed25519Seed) != ed25519.SeedSize {
		return Recovery{}, fmt.Errorf("%w: ed25519 key", ErrFormat)
	}
	return Recovery{X25519: x, Ed25519: ed25519.NewKeyFromSeed(rp.Ed25519Seed)}, nil
}

// MarshalPublic returns the public halves as JSON.
func (p RecoveryPublic) MarshalPublic() ([]byte, error) {
	return json.Marshal(p)
}

// ParseRecoveryPublic reads what MarshalPublic wrote and checks both keys parse.
func ParseRecoveryPublic(data []byte) (RecoveryPublic, error) {
	var p RecoveryPublic
	if err := json.Unmarshal(data, &p); err != nil {
		return RecoveryPublic{}, fmt.Errorf("parse recovery public keys: %w", err)
	}
	if _, err := p.SealKey(); err != nil {
		return RecoveryPublic{}, err
	}
	if len(p.Ed25519) != ed25519.PublicKeySize {
		return RecoveryPublic{}, fmt.Errorf("%w: ed25519 key", ErrFormat)
	}
	return p, nil
}

// SealKey returns the X25519 public key a backup is sealed to.
func (p RecoveryPublic) SealKey() (*ecdh.PublicKey, error) {
	k, err := ecdh.X25519().NewPublicKey(p.X25519)
	if err != nil {
		return nil, fmt.Errorf("%w: x25519 key", ErrFormat)
	}
	return k, nil
}
