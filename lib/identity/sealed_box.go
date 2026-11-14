package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// ErrOpen is returned when a sealed box does not open: wrong key, wrong info
// or a changed box.
var ErrOpen = errors.New("sealed box does not open")

// Seal encrypts plaintext to pub with a fresh X25519 key, HKDF-SHA256 and
// AES-256-GCM. info binds the box to its purpose: Open fails under another.
// The result is the ephemeral public key, the nonce, then the ciphertext.
func Seal(pub *ecdh.PublicKey, plaintext, info []byte) ([]byte, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	gcm, err := boxCipher(eph, pub, eph.PublicKey(), pub, info)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	out := append(eph.PublicKey().Bytes(), nonce...)
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

// Open decrypts a box Seal made to priv's public half under the same info.
func Open(priv *ecdh.PrivateKey, sealed, info []byte) ([]byte, error) {
	const keySize = 32
	if len(sealed) < keySize {
		return nil, ErrOpen
	}
	eph, err := ecdh.X25519().NewPublicKey(sealed[:keySize])
	if err != nil {
		return nil, ErrOpen
	}
	gcm, err := boxCipher(priv, eph, eph, priv.PublicKey(), info)
	if err != nil {
		return nil, ErrOpen
	}
	rest := sealed[keySize:]
	if len(rest) < gcm.NonceSize() {
		return nil, ErrOpen
	}
	pt, err := gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], nil)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// boxCipher derives the box key from the shared secret of local and remote;
// the salt is the ephemeral and recipient public keys.
func boxCipher(local *ecdh.PrivateKey, remote *ecdh.PublicKey, ephPub, recipient *ecdh.PublicKey, info []byte) (cipher.AEAD, error) {
	secret, err := local.ECDH(remote)
	if err != nil {
		return nil, fmt.Errorf("key agreement: %w", err)
	}
	salt := append(ephPub.Bytes(), recipient.Bytes()...)
	key, err := hkdf.Key(sha256.New, secret, salt, string(info), 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	return cipher.NewGCM(blk)
}
