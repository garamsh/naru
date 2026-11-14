package orchapi

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The headers of a poll of an enrollment.
const (
	HeaderTime      = "X-Naru-Time"
	HeaderSignature = "X-Naru-Signature"
)

// ProofSkew is how far a poll's time may differ from the clock of the orch that checks it.
const ProofSkew = 5 * time.Minute

func proofMessage(hostID string, unix int64) []byte {
	return []byte("naru-enroll-poll\n" + hostID + "\n" + strconv.FormatInt(unix, 10))
}

// SignProof signs a poll of hostID's enrollment at now with the host's key, which must be ECDSA or
// Ed25519. It returns the values of HeaderTime and HeaderSignature.
func SignProof(key crypto.Signer, hostID string, now time.Time) (timeValue, signature string, err error) {
	msg := proofMessage(hostID, now.Unix())
	var sig []byte
	switch key.Public().(type) {
	case *ecdsa.PublicKey:
		sum := sha256.Sum256(msg)
		sig, err = key.Sign(rand.Reader, sum[:], crypto.SHA256)
	case ed25519.PublicKey:
		sig, err = key.Sign(rand.Reader, msg, crypto.Hash(0))
	default:
		err = fmt.Errorf("a %T key cannot sign a poll", key.Public())
	}
	if err != nil {
		return "", "", err
	}
	return strconv.FormatInt(now.Unix(), 10), base64.StdEncoding.EncodeToString(sig), nil
}

// ErrBadProof is the answer to a poll whose signature or time does not hold.
var ErrBadProof = errors.New("the proof of the host's key does not hold")

// VerifyProof checks a poll against the host's stored PKIX DER public key.
func VerifyProof(publicKeyDER []byte, hostID, timeValue, signature string, now time.Time) error {
	unix, err := strconv.ParseInt(timeValue, 10, 64)
	if err != nil {
		return ErrBadProof
	}
	if d := now.Sub(time.Unix(unix, 0)); d > ProofSkew || d < -ProofSkew {
		return ErrBadProof
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return ErrBadProof
	}
	pub, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return ErrBadProof
	}
	msg := proofMessage(hostID, unix)
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		sum := sha256.Sum256(msg)
		if ecdsa.VerifyASN1(k, sum[:], sig) {
			return nil
		}
	case ed25519.PublicKey:
		if ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return ErrBadProof
}
