package orchd_test

import (
	"crypto/tls"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// presented is the certificate the listener at addr presents.
func presented(t *testing.T, addr string) []byte {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}) //nolint:gosec // the test reads the certificate, and trusts nothing
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].Raw
}

// replace puts the file at from in place of the file at to whole, as a renewal writes it.
func replace(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to+".new", b, 0o600))
	require.NoError(t, os.Rename(to+".new", to))
}

func TestAMemberPresentsItsRenewedPairAndTrustsItsStoreFromTheNextHandshakeWithoutARestart(t *testing.T) {
	o := startOne(t, orchtest.Options{CAStore: true, MachineZone: "garam.test"})
	m := o.c.Members[0]
	files := o.c.MemberConfig(0)
	first, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	require.NoError(t, err)
	assert.Equal(t, first.Certificate[0], presented(t, m.Addr()), "setup: the API presents the first pair")

	next := o.c.CA.IssueFiles(t, t.TempDir(), "m0")
	replace(t, next.Key, files.Key)
	replace(t, next.Cert, files.Cert)
	renewed, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	require.NoError(t, err)
	require.NotEqual(t, first.Certificate[0], renewed.Certificate[0])
	assert.Equal(t, renewed.Certificate[0], presented(t, m.Addr()), "the API presents the renewed pair")
	assert.Equal(t, renewed.Certificate[0], presented(t, m.MachineAPIAddr()), "and so does the Machine API")

	other := orchtest.NewCA(t)
	rita := other.Issue(t, "rita", identity.RoleReader)
	_, err = o.c.Client(t, 0, &rita).Events(t.Context())
	require.Error(t, err, "an authority the member does not hold")
	require.NoError(t, os.WriteFile(o.c.MemberStore(0), other.PEM(), 0o644))
	_, err = o.c.Client(t, 0, &rita).Events(t.Context())
	require.NoError(t, err, "control: the same caller, once the store holds its authority")
}
