//go:build qemu_real

package tests_test

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
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

const (
	testIssuer      = "https://oidc.example.com/naru"
	inClusterIssuer = "https://kubernetes.default.svc.cluster.local"
)

// TestNode_AServiceAccountTokenCarriesTheConfiguredIssuerAndVerifiesAgainstNaructlsKeySetRS256 starts a
// cluster whose first ServiceAccount issuer is an operator's and whose key is the default (ADR 0087).
func TestNode_AServiceAccountTokenCarriesTheConfiguredIssuerAndVerifiesAgainstNaructlsKeySetRS256(t *testing.T) {
	serviceAccountIssuerCluster(t, "")
}

// TestNode_AServiceAccountTokenCarriesTheConfiguredIssuerAndVerifiesAgainstNaructlsKeySetES256 is the same
// with the key an ECDSA P-256 one.
func TestNode_AServiceAccountTokenCarriesTheConfiguredIssuerAndVerifiesAgainstNaructlsKeySetES256(t *testing.T) {
	serviceAccountIssuerCluster(t, "ES256")
}

// serviceAccountIssuerCluster checks, on a cluster of alg (the default when empty), that a projected
// token's iss is the first issuer, that its signature verifies against the key set naructl wrote, that
// a token of the in-cluster issuer is still accepted, and that one of an issuer the cluster does not
// list is not.
func serviceAccountIssuerCluster(t *testing.T, alg string) {
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	spec := node.KubeSpec{
		Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs, Naructl: tool,
		ServiceAccountIssuers: []string{testIssuer}, ServiceAccountAlgorithm: alg,
	}
	c, err := node.StartKubernetes(spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		for _, nd := range c.Nodes {
			t.Logf("console of %s:\n%s", nd.Name, nd.Console())
		}
	})
	require.Eventually(t, func() bool { return len(readyNodes(t, c)) == 2 }, kubeTimeout, 3*time.Second, "node-1 and node-2 are Ready")

	var token string
	require.Eventually(t, func() bool {
		out, err := kubectl(t, c, "", "create", "token", "default", "-n", "default")
		token = out
		return err == nil && strings.Count(out, ".") == 2
	}, kubeTimeout, 3*time.Second, "the API server issues a token")
	header, claims := jwtParts(t, token)
	assert.Equal(t, testIssuer, claims["iss"], "the first issuer signs")

	out := t.TempDir()
	res := tool.Run("gen", "kubernetes-jwks", "--ca-dir", c.CADir, "--out-dir", out)
	require.NoError(t, res.Err, res.Stderr)
	set, err := os.ReadFile(filepath.Join(out, "naru", "openid", "v1", "jwks"))
	require.NoError(t, err)
	want := map[string]string{"": "RS256", "ES256": "ES256"}[alg]
	assert.Equal(t, want, header["alg"], "the key decides the algorithm")
	assert.NoError(t, verifyAgainstJWKS(token, set), "the signature verifies against naructl's key set")

	signer := signingKey(t, filepath.Join(c.CADir, "sa.key"))
	inCluster := resign(t, token, signer, inClusterIssuer)
	assert.True(t, reviewed(t, c, inCluster), "a token of the in-cluster issuer is still accepted")
	assert.True(t, reviewed(t, c, token), "control: the configured issuer's token is accepted")
	assert.False(t, reviewed(t, c, resign(t, token, signer, "https://unlisted.example.com")), "control: an issuer the cluster does not list is refused")
}

// reviewed is whether the API server authenticates token, by a TokenReview.
func reviewed(t *testing.T, c *node.KubeCluster, token string) bool {
	t.Helper()
	review, err := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview", "spec": map[string]any{"token": token},
	})
	require.NoError(t, err)
	out, err := kubectl(t, c, string(review), "create", "-f", "-", "-o", "jsonpath={.status.authenticated}")
	require.NoError(t, err, out)
	return out == "true"
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwtParts(t *testing.T, token string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	for i, into := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, into))
	}
	return header, claims
}

// signingKey reads the ServiceAccount signing key gen kubernetes wrote.
func signingKey(t *testing.T, file string) crypto.Signer {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	block, _ := pem.Decode(b)
	require.NotNil(t, block)
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		require.NoError(t, err)
		return k
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		require.NoError(t, err)
		return k
	}
	require.FailNow(t, "unknown key type "+block.Type)
	return nil
}

// resign has token's claims, with iss replaced, signed by signer with the kid of its public key.
func resign(t *testing.T, token string, signer crypto.Signer, iss string) string {
	t.Helper()
	header, claims := jwtParts(t, token)
	claims["iss"] = iss
	// The header names the signer's own key, as the API server's would.
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	require.NoError(t, err)
	sum := sha256.Sum256(der)
	header["kid"] = b64(sum[:])
	if _, ok := signer.(*rsa.PrivateKey); ok {
		header["alg"] = "RS256"
	} else {
		header["alg"] = "ES256"
	}
	rawHeader, err := json.Marshal(header)
	require.NoError(t, err)
	rawClaims, err := json.Marshal(claims)
	require.NoError(t, err)
	signing := b64(rawHeader) + "." + b64(rawClaims)
	digest := sha256.Sum256([]byte(signing))
	var sig []byte
	switch k := signer.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
		require.NoError(t, err)
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		require.NoError(t, err)
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	}
	return signing + "." + b64(sig)
}

// verifyAgainstJWKS checks token's signature with the key of set its kid names.
func verifyAgainstJWKS(token string, set []byte) error {
	var jwks struct {
		Keys []struct {
			Kid, Kty, Crv, N, E, X, Y string
		}
	}
	if err := json.Unmarshal(set, &jwks); err != nil {
		return err
	}
	parts := strings.Split(token, ".")
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return err
	}
	var header struct{ Kid, Alg string }
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	num := func(s string) *big.Int {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		return new(big.Int).SetBytes(b)
	}
	for _, k := range jwks.Keys {
		if k.Kid != header.Kid {
			continue
		}
		switch {
		case k.Kty == "RSA" && header.Alg == "RS256":
			return rsa.VerifyPKCS1v15(&rsa.PublicKey{N: num(k.N), E: int(num(k.E).Int64())}, crypto.SHA256, digest[:], sig)
		case k.Kty == "EC" && header.Alg == "ES256" && len(sig) == 64:
			if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: num(k.X), Y: num(k.Y)}, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
				return errBadSignature
			}
			return nil
		}
		return errBadSignature
	}
	return errNoKey
}

type testError string

func (e testError) Error() string { return string(e) }

const (
	errBadSignature = testError("the signature does not verify")
	errNoKey        = testError("the key set has no key of the token's kid")
)

// TestNode_ATokenOfTheKeyBeforeARotationStillVerifiesAndOneOfTheNewKeyDoes starts a cluster whose
// ServiceAccount key was rotated once before it booted (ADR 0087): sa.pub holds the new key and the
// old one. A token signed by the old key is accepted by the API server and verifies against naructl's key
// set, as does the token the API server issues now, and one signed by a key the cluster never had is
// refused.
func TestNode_ATokenOfTheKeyBeforeARotationStillVerifiesAndOneOfTheNewKeyDoes(t *testing.T) {
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	spec := node.KubeSpec{
		Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs, Naructl: tool,
		ServiceAccountIssuers: []string{testIssuer}, RotateServiceAccountKey: true,
	}
	c, err := node.StartKubernetes(spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		for _, nd := range c.Nodes {
			t.Logf("console of %s:\n%s", nd.Name, nd.Console())
		}
	})
	require.Eventually(t, func() bool { return len(readyNodes(t, c)) == 2 }, kubeTimeout, 3*time.Second, "node-1 and node-2 are Ready")

	var token string
	require.Eventually(t, func() bool {
		out, err := kubectl(t, c, "", "create", "token", "default", "-n", "default")
		token = out
		return err == nil && strings.Count(out, ".") == 2
	}, kubeTimeout, 3*time.Second, "the API server issues a token")
	out := t.TempDir()
	res := tool.Run("gen", "kubernetes-jwks", "--ca-dir", c.CADir, "--out-dir", out)
	require.NoError(t, res.Err, res.Stderr)
	set, err := os.ReadFile(filepath.Join(out, "naru", "openid", "v1", "jwks"))
	require.NoError(t, err)
	var keys struct{ Keys []json.RawMessage }
	require.NoError(t, json.Unmarshal(set, &keys))
	assert.Len(t, keys.Keys, 2, "the key set holds the new key and the old one")

	old := signingKey(t, filepath.Join(c.CADir, "sa.key.previous"))
	oldToken := resign(t, token, old, testIssuer)
	assert.NoError(t, verifyAgainstJWKS(token, set), "the new key's token verifies against the key set")
	assert.NoError(t, verifyAgainstJWKS(oldToken, set), "the old key's token verifies against the key set")
	assert.True(t, reviewed(t, c, token), "the API server accepts the new key's token")
	assert.True(t, reviewed(t, c, oldToken), "the API server accepts the token the old key signed")
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	assert.False(t, reviewed(t, c, resign(t, token, stranger, testIssuer)), "control: a key the cluster never had is refused")
}
