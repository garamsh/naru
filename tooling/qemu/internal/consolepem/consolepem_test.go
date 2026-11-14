package consolepem_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/consolepem"
)

func TestExtract_ReadsABlockWhoseLinesCarryALogPrefixAndSkipsOtherTypesAndACutOffBlock(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	require.NoError(t, err)
	block := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	var console strings.Builder
	console.WriteString("[   1.0] naru-signer: revocation public key\n")
	for _, l := range strings.Split(strings.TrimSpace(block), "\n") {
		console.WriteString("[   1.1] naru-signer: " + l + "\n")
	}
	console.WriteString("naru-signer: -----BEGIN CERTIFICATE-----\nnaru-signer: MIIB\n")

	got := consolepem.Extract(console.String(), "PUBLIC KEY")

	assert.Equal(t, []string{block}, got)
	assert.Empty(t, consolepem.Extract(console.String(), "CERTIFICATE"), "a block with no END line is not returned")
}
