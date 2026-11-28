package api

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

func TestHost_ReportsWhenItsClientCAPoolStopsTrustingAnAuthorityOnEveryRequest(t *testing.T) {
	file := filepath.Join(t.TempDir(), "client-ca.crt")
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.ClientCAFile = file })
	c := e.as(t, ReaderOrganization)
	get := func() hostapi.Host {
		code, b := e.do(t, c, "GET", "/v1/host", nil)
		require.Equal(t, 200, code, string(b))
		var h hostapi.Host
		require.NoError(t, json.Unmarshal(b, &h))
		return h
	}

	assert.Nil(t, get().TrustExpiresAt, "a pool hostd cannot read reports nothing, and the host still answers")

	require.NoError(t, os.WriteFile(file, e.ca.pem, 0o600))
	h := get()
	require.NotNil(t, h.TrustExpiresAt)
	assert.True(t, h.TrustExpiresAt.Equal(e.ca.cert.NotAfter))

	// The authority signed again under its key: a pool that took the new certificate reports its end.
	tmpl := *e.ca.cert
	tmpl.SerialNumber, tmpl.NotAfter = big.NewInt(2), e.ca.cert.NotAfter.AddDate(10, 0, 0)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &e.ca.key.PublicKey, e.ca.key)
	require.NoError(t, err)
	renewed := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(file, append(append([]byte{}, e.ca.pem...), renewed...), 0o600))
	h = get()
	require.NotNil(t, h.TrustExpiresAt)
	assert.True(t, h.TrustExpiresAt.Equal(tmpl.NotAfter.Truncate(time.Second)), "got %s", h.TrustExpiresAt)
}
