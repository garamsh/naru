package main

import (
	"crypto/x509/pkix"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/api"
	"github.com/garamsh/naru/host/internal/network"
)

func TestImporter_ReachesAPeerAtItsMeshAddressOnThisHostsAPIPort(t *testing.T) {
	authority := newCA(t)
	dir := t.TempDir()
	ca := filepath.Join(dir, "client-ca.crt")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.cert.Raw}), 0o600))
	certPEM, keyPEM, _, leaf := authority.issue(t, pkix.Name{CommonName: "h-tgt"}, true)
	key := filepath.Join(dir, "host.key")
	require.NoError(t, os.WriteFile(key, keyPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host.crt"), certPEM, 0o600))
	peers := []network.Peer{
		{Name: "h-src", Mesh: netip.MustParseAddr("10.99.0.2")},
		{Name: "h-nomesh"},
	}
	pool, err := api.NewClientPool(ca, "")
	require.NoError(t, err)
	imp, err := newImporter(key, pool, "0.0.0.0:7443", func() []network.Peer { return peers })
	require.NoError(t, err)
	roots, _ := pool.Pool()
	assert.Same(t, roots, imp.Roots(), "the roots are the client CA pool as it stands")

	pair, err := imp.Pair()
	require.NoError(t, err)
	assert.Equal(t, leaf.Raw, pair.Certificate[0], "the pair is the one beside the host's key")

	addr, err := imp.Address("h-src")
	require.NoError(t, err)
	assert.Equal(t, "10.99.0.2:7443", addr)
	_, err = imp.Address("h-nomesh")
	assert.Error(t, err, "a peer without a mesh address is not reached over the LAN")
	_, err = imp.Address("h-unknown")
	assert.Error(t, err)

	_, err = newImporter("k", pool, "0.0.0.0", func() []network.Peer { return nil })
	assert.Error(t, err, "a listen address without a port")
}
