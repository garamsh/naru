package pki_test

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout/pki"
)

func privateKey(t *testing.T, keyPEM []byte) crypto.Signer {
	t.Helper()
	block, _ := pem.Decode(keyPEM)
	require.NotNil(t, block)
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k
	}
	k, err := x509.ParseECPrivateKey(block.Bytes)
	require.NoError(t, err)
	return k
}

func TestServiceAccountKey_IsOfTheAlgorithmAskedAndItsPublicKeyIsItsOwn(t *testing.T) {
	for _, alg := range []pki.Algorithm{pki.RS256, pki.ES256} {
		keyPEM, pubPEM, err := pki.ServiceAccountKey(alg)
		require.NoError(t, err, alg)

		key := privateKey(t, keyPEM)
		pubs, err := pki.PublicKeys(pubPEM)
		require.NoError(t, err, alg)
		require.Len(t, pubs, 1, alg)
		assert.True(t, key.Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(pubs[0]), alg)
		got, err := pki.KeyAlgorithm(pubPEM)
		require.NoError(t, err)
		assert.Equal(t, alg, got)
		switch alg {
		case pki.RS256:
			assert.Equal(t, 2048, key.(*rsa.PrivateKey).N.BitLen())
		case pki.ES256:
			assert.Equal(t, "P-256", key.(*ecdsa.PrivateKey).Curve.Params().Name)
		}
	}
	_, _, err := pki.ServiceAccountKey("HS256")
	assert.Error(t, err)
}

func TestParseAlgorithm_AcceptsRS256AndES256Only(t *testing.T) {
	for _, s := range []string{"RS256", "ES256"} {
		a, err := pki.ParseAlgorithm(s)
		require.NoError(t, err)
		assert.Equal(t, pki.Algorithm(s), a)
	}
	_, err := pki.ParseAlgorithm("rs256")
	assert.Error(t, err)
	assert.Equal(t, pki.RS256, pki.DefaultAlgorithm)
}

func TestServiceAccountJWKS_RSAKeyIsTheKeysModulusAndExponentUnderTheAPIServersKeyID(t *testing.T) {
	_, pubPEM, err := pki.ServiceAccountKey(pki.RS256)
	require.NoError(t, err)
	pubs, _ := pki.PublicKeys(pubPEM)
	pub := pubs[0].(*rsa.PublicKey)

	jwks, err := pki.ServiceAccountJWKS(pubPEM)
	require.NoError(t, err)

	var set struct{ Keys []map[string]string }
	require.NoError(t, json.Unmarshal(jwks, &set))
	require.Len(t, set.Keys, 1)
	k := set.Keys[0]
	assert.Equal(t, "RSA", k["kty"])
	assert.Equal(t, "RS256", k["alg"])
	assert.Equal(t, "sig", k["use"])
	n, err := base64.RawURLEncoding.DecodeString(k["n"])
	require.NoError(t, err)
	assert.Equal(t, pub.N, new(big.Int).SetBytes(n))
	assert.Equal(t, "AQAB", k["e"], "65537")
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), k["kid"], "the kid kube-apiserver puts in a token's header")
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
		assert.NotContains(t, k, private)
	}
}

func TestServiceAccountJWKS_ECKeyIsTheCurveAndThe32ByteCoordinates(t *testing.T) {
	_, pubPEM, err := pki.ServiceAccountKey(pki.ES256)
	require.NoError(t, err)
	pubs, _ := pki.PublicKeys(pubPEM)
	pub := pubs[0].(*ecdsa.PublicKey)

	jwks, err := pki.ServiceAccountJWKS(pubPEM)
	require.NoError(t, err)

	var set struct{ Keys []map[string]string }
	require.NoError(t, json.Unmarshal(jwks, &set))
	require.Len(t, set.Keys, 1)
	k := set.Keys[0]
	assert.Equal(t, "EC", k["kty"])
	assert.Equal(t, "P-256", k["crv"])
	assert.Equal(t, "ES256", k["alg"])
	x, _ := base64.RawURLEncoding.DecodeString(k["x"])
	y, _ := base64.RawURLEncoding.DecodeString(k["y"])
	assert.Len(t, x, 32)
	assert.Len(t, y, 32)
	assert.Equal(t, pub.X, new(big.Int).SetBytes(x))
	assert.Equal(t, pub.Y, new(big.Int).SetBytes(y))
	assert.NotContains(t, k, "d")
}

func TestRotatedPublicKeys_NewKeySignsAndOnlyTheKeyBeforeItStaysAccepted(t *testing.T) {
	_, first, _ := pki.ServiceAccountKey(pki.ES256)
	_, second, _ := pki.ServiceAccountKey(pki.RS256)
	_, third, _ := pki.ServiceAccountKey(pki.RS256)

	afterOne, err := pki.RotatedPublicKeys(second, first)
	require.NoError(t, err)
	afterTwo, err := pki.RotatedPublicKeys(third, afterOne)
	require.NoError(t, err)

	keys, err := pki.PublicKeys(afterOne)
	require.NoError(t, err)
	assert.Len(t, keys, 2)
	alg, _ := pki.KeyAlgorithm(afterOne)
	assert.Equal(t, pki.RS256, alg, "the new key is first, and the first signs")
	twice, err := pki.PublicKeys(afterTwo)
	require.NoError(t, err)
	require.Len(t, twice, 2, "the first key is dropped by the second rotation")
	jwks, _ := pki.ServiceAccountJWKS(afterTwo)
	oldKid, _ := pki.KeyID(keys[1])
	assert.NotContains(t, string(jwks), oldKid)
}

func TestPublicKeys_RefusesAWeakOrUnsupportedKeyAndAnEmptyFile(t *testing.T) {
	_, err := pki.PublicKeys(nil)
	assert.Error(t, err)
	_, err = pki.PublicKeys([]byte("pub"))
	assert.Error(t, err)

	weakRSA, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	_, err = pki.PublicKeys(pubPEM(t, &weakRSA.PublicKey))
	assert.ErrorContains(t, err, "1024 bits")
	_, err = pki.PublicKeys(pubPEM(t, &p384.PublicKey))
	assert.ErrorContains(t, err, "RSA or ECDSA P-256")
	ok, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = pki.PublicKeys(pubPEM(t, &ok.PublicKey))
	assert.NoError(t, err, "control: RSA 2048")
	_, err = pki.PublicKeys(pubPEM(t, &p256.PublicKey))
	assert.NoError(t, err, "control: P-256")
}

func pubPEM(t *testing.T, pub crypto.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestSigningPublicKey_IsTheSigningKeysPublicKeyAndRefusesAWeakOrForeignKey(t *testing.T) {
	for _, alg := range []pki.Algorithm{pki.RS256, pki.ES256} {
		key, pub, err := pki.ServiceAccountKey(alg)
		require.NoError(t, err)
		got, err := pki.SigningPublicKey(key)
		require.NoError(t, err)
		assert.Equal(t, pub, got, string(alg))
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	_, err = pki.SigningPublicKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)}))
	assert.ErrorContains(t, err, "1024 bits")
	_, err = pki.SigningPublicKey([]byte("key"))
	assert.Error(t, err)
}

func TestOIDCDiscovery_NamesTheIssuerTheKeySetAndTheAlgorithmsOfTheKeys(t *testing.T) {
	_, es, _ := pki.ServiceAccountKey(pki.ES256)
	_, rs, _ := pki.ServiceAccountKey(pki.RS256)
	both, err := pki.RotatedPublicKeys(rs, es)
	require.NoError(t, err)

	doc, err := pki.OIDCDiscovery("https://oidc.example.com", "https://oidc.example.com/openid/v1/jwks", both)
	require.NoError(t, err)

	var got struct {
		Algs     []string `json:"id_token_signing_alg_values_supported"`
		Response []string `json:"response_types_supported"`
		Subject  []string `json:"subject_types_supported"`
		I        string   `json:"issuer"`
		J        string   `json:"jwks_uri"`
	}
	require.NoError(t, json.Unmarshal(doc, &got))
	assert.Equal(t, "https://oidc.example.com", got.I)
	assert.Equal(t, "https://oidc.example.com/openid/v1/jwks", got.J)
	assert.Equal(t, []string{"RS256", "ES256"}, got.Algs)
	assert.Equal(t, []string{"id_token"}, got.Response)
	assert.Equal(t, []string{"public"}, got.Subject)
}
