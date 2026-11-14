package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

// ErrPassphrase is returned when a file does not open: the passphrase is
// wrong or the file was changed. The two are not told apart.
var ErrPassphrase = errors.New("wrong passphrase or damaged file")

// ErrFormat is returned when a file is not one this version writes.
var ErrFormat = errors.New("not a naru encrypted file of a known version")

const (
	fileVersion = "1"
	scryptN     = 1 << 15
	scryptR     = 8
	scryptP     = 1
	saltSize    = 16
)

// sealFile encrypts plaintext under a key scrypt derives from passphrase and
// returns it as a PEM block of blockType. The block type and version are
// authenticated with the ciphertext.
func sealFile(blockType, passphrase string, plaintext []byte) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	gcm, err := fileCipher(passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	body := gcm.Seal(nonce, nonce, plaintext, fileAAD(blockType))
	return pem.EncodeToMemory(&pem.Block{
		Type: blockType,
		Headers: map[string]string{
			"Version": fileVersion,
			"KDF":     "scrypt",
			"Salt":    base64.StdEncoding.EncodeToString(salt),
		},
		Bytes: body,
	}), nil
}

// openFile reverses sealFile. The cost parameters are fixed, not read from the
// file, so a crafted file cannot ask for more work.
func openFile(blockType, passphrase string, data []byte) ([]byte, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != blockType || block.Headers["Version"] != fileVersion || block.Headers["KDF"] != "scrypt" {
		return nil, ErrFormat
	}
	salt, err := base64.StdEncoding.DecodeString(block.Headers["Salt"])
	if err != nil || len(salt) != saltSize {
		return nil, ErrFormat
	}
	gcm, err := fileCipher(passphrase, salt)
	if err != nil {
		return nil, err
	}
	if len(block.Bytes) < gcm.NonceSize() {
		return nil, ErrFormat
	}
	nonce, ct := block.Bytes[:gcm.NonceSize()], block.Bytes[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, fileAAD(blockType))
	if err != nil {
		return nil, ErrPassphrase
	}
	return pt, nil
}

func fileCipher(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	return cipher.NewGCM(blk)
}

func fileAAD(blockType string) []byte {
	return []byte(blockType + "/" + fileVersion)
}
