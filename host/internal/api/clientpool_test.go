package api

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedAgain is the authority's certificate signed again under its key, valid from now-2h until notAfter.
func (a authority) signedAgain(t *testing.T, serial int64, notAfter time.Time) []byte {
	t.Helper()
	tpl := *a.cert
	tpl.SerialNumber, tpl.NotBefore, tpl.NotAfter = big.NewInt(serial), time.Now().Add(-2*time.Hour), notAfter
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &a.key.PublicKey, a.key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// handshake reports the server's verdict on a client presenting pair to conf.
func handshake(t *testing.T, conf *tls.Config, pair tls.Certificate) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", conf)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	verdict := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			verdict <- err
			return
		}
		defer func() { _ = c.Close() }()
		verdict <- c.(*tls.Conn).Handshake()
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{pair}}) //nolint:gosec // the test checks the server's verdict on the client
	if err == nil {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = c.Read(make([]byte, 1))
		_ = c.Close()
	}
	return <-verdict
}

func TestTheHostAPITrustsAnAuthoritysReSigningTheAgentStoredFromTheNextHandshake(t *testing.T) {
	dir := t.TempDir()
	ca, srv := newAuthority(t, "clients"), newAuthority(t, "server")
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	_, crt, key := srv.issue(t, nil, true)
	serving, err := NewServing(write("tls.crt", crt), write("tls.key", key), filepath.Join(dir, "serving.pem"))
	require.NoError(t, err)
	client, _, _ := ca.issue(t, []string{ReaderOrganization}, false)

	// --client-ca holds the authority's certificate that has ended; the agent has stored nothing yet.
	refreshed := filepath.Join(dir, "client-ca.pem")
	pool, err := NewClientPool(write("client-ca.crt", ca.signedAgain(t, 2, time.Now().Add(-time.Minute))), refreshed)
	require.NoError(t, err)
	conf := TLSConfig(serving, pool, nil)
	require.Error(t, handshake(t, conf, client), "the only certificate of the authority has ended")

	require.NoError(t, os.WriteFile(refreshed, ca.signedAgain(t, 3, time.Now().Add(time.Hour)), 0o600))
	assert.NoError(t, handshake(t, conf, client), "the re-signing the agent stored, with no restart")

	require.NoError(t, os.WriteFile(refreshed, nil, 0o600))
	assert.Error(t, handshake(t, conf, client), "and the pool follows the file")
}

func TestAPoolWhoseFilesHoldNoCertificateIsRefusedAndAnUnreadableChangeKeepsTheLast(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "client-ca.crt")
	require.NoError(t, os.WriteFile(base, []byte("no certificate"), 0o600))
	_, err := NewClientPool(base, filepath.Join(dir, "absent"))
	require.Error(t, err)

	ca := newAuthority(t, "clients")
	require.NoError(t, os.WriteFile(base, ca.pem, 0o600))
	p, err := NewClientPool(base, filepath.Join(dir, "absent"))
	require.NoError(t, err, "an absent store adds nothing")
	first, err := p.Pool()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(base, []byte("torn"), 0o600))
	got, err := p.Pool()
	assert.Error(t, err)
	assert.Same(t, first, got, "a pool that cannot be read keeps the one last read")
}

func TestAHandshakeReadsThePoolsFilesOnlyWhenTheyHaveChanged(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "client-ca.crt")
	require.NoError(t, os.WriteFile(base, newAuthority(t, "clients").pem, 0o600))
	p, err := NewClientPool(base, filepath.Join(dir, "client-ca.pem"))
	require.NoError(t, err)
	first, err := p.Pool()
	require.NoError(t, err)

	// A file hostd can no longer read, with its size and modification time unchanged, is not read.
	require.NoError(t, os.Chmod(base, 0))
	t.Cleanup(func() { _ = os.Chmod(base, 0o600) })
	if _, err := os.ReadFile(base); err == nil {
		t.Skip("the test runs as a user every file is readable to")
	}
	got, err := p.Pool()
	require.NoError(t, err, "the file was not read")
	assert.Same(t, first, got)

	require.NoError(t, os.Chmod(base, 0o600))
	require.NoError(t, os.WriteFile(base, newAuthority(t, "other").pem, 0o600))
	got, err = p.Pool()
	require.NoError(t, err)
	assert.NotSame(t, first, got, "control: a file that changed is read")
}
