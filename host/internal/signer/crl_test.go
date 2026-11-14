package signer_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// replica returns a second signer holding what e holds, made the way a lost signer is restored:
// from a bundle sealed to the recovery key.
func replica(t *testing.T, e *env) *signer.Signer {
	t.Helper()
	bundle, err := e.s.Bundle(e.signed(e.rootID, identity.ActionBundle, struct{}{}))
	require.NoError(t, err)
	sum := sha256.Sum256(bundle)
	authz := e.signed(e.rootID, identity.ActionRestore, identity.RestoreRequest{BundleSHA256: hex.EncodeToString(sum[:])})
	s := openAt(t, privateDir(t, t.TempDir()))
	require.NoError(t, s.Restore(identity.RestoreBody{Bundle: bundle, Authorisation: authz, RecoveryKey: e.recoveryFile, PassphraseFile: e.passFile}))
	return s
}

func admit(t *testing.T, e *env, s *signer.Signer, by person) error {
	t.Helper()
	var req identity.HostRequest
	require.NoError(t, json.Unmarshal(e.hostRequest(by, "host-1", newKey(t), time.Now().Add(time.Hour)), &req))
	_, err := s.IssueHost(req)
	return err
}

func serialsOf(t *testing.T, e *env, s *signer.Signer) ([]string, uint64) {
	t.Helper()
	data, err := s.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), data)
	require.NoError(t, err)
	return crl.Serials, crl.Seq
}

// sync has every signer take every other's list, twice, the way orch's loop would carry them.
func syncAll(t *testing.T, signers ...*signer.Signer) {
	t.Helper()
	for range 2 {
		for _, from := range signers {
			for _, to := range signers {
				if from == to {
					continue
				}
				data, err := from.CRL()
				require.NoError(t, err)
				_, err = to.PushCRL(data)
				require.NoError(t, err)
			}
		}
	}
}

func TestCRLSync_ARevokeOnOneSignerReachesTheOthersAfterASync(t *testing.T) {
	a := newEnv(t)
	op := a.issue(identity.RoleOperator, "alice")
	b, c := replica(t, a), replica(t, a)
	for _, s := range []*signer.Signer{a.s, b, c} {
		require.NoError(t, admit(t, a, s, op), "control: before the revocation every signer admits for alice")
	}

	status, body := a.revoke(a.rootID, op.cert.SerialNumber.Text(16))
	require.Equal(t, http.StatusOK, status, string(body))
	require.ErrorIs(t, admit(t, a, a.s, op), identity.ErrRevoked)
	require.NoError(t, admit(t, a, b, op), "before a sync B does not know")
	require.NoError(t, admit(t, a, c, op), "before a sync C does not know")

	got, err := a.s.CRL()
	require.NoError(t, err)
	for _, s := range []*signer.Signer{b, c} {
		held, err := s.PushCRL(got)
		require.NoError(t, err)
		assert.Equal(t, got, held, "the signer returns the list it now holds")
		require.ErrorIs(t, admit(t, a, s, op), identity.ErrRevoked)
	}
}

func TestCRLSync_ListsThatEachRevokedSomethingAreMergedAndSignedAgain(t *testing.T) {
	a := newEnv(t)
	ops := []person{a.issue(identity.RoleOperator, "x"), a.issue(identity.RoleOperator, "y"), a.issue(identity.RoleOperator, "z")}
	b, c := replica(t, a), replica(t, a)
	revokeOn := func(s *signer.Signer, p person) {
		_, err := s.Revoke(a.signed(a.rootID, identity.ActionRevoke, identity.RevokeRequest{Serial: p.cert.SerialNumber.Text(16)}))
		require.NoError(t, err)
	}
	revokeOn(a.s, ops[0])
	revokeOn(b, ops[1])
	revokeOn(c, ops[2])
	for _, s := range []*signer.Signer{a.s, b, c} {
		_, seq := serialsOf(t, a, s)
		require.Equal(t, uint64(2), seq, "control: each holds its own revocation at the same sequence")
	}

	syncAll(t, a.s, b, c)

	var want []string
	for _, p := range ops {
		want = append(want, p.cert.SerialNumber.Text(16))
	}
	for _, s := range []*signer.Signer{a.s, b, c} {
		got, seq := serialsOf(t, a, s)
		assert.ElementsMatch(t, want, got)
		assert.Greater(t, seq, uint64(2), "a merged list is signed again at a higher sequence")
		for _, p := range ops {
			require.ErrorIs(t, admit(t, a, s, p), identity.ErrRevoked)
		}
	}
	reopened := openAt(t, a.dir)
	got, _ := serialsOf(t, a, reopened)
	assert.ElementsMatch(t, want, got, "the merged list is kept in the directory")
}

func TestCRLSync_AListThatAddsNothingOrIsOlderChangesNothing(t *testing.T) {
	a := newEnv(t)
	old, err := a.s.CRL()
	require.NoError(t, err)
	op := a.issue(identity.RoleOperator, "alice")
	status, _ := a.revoke(a.rootID, op.cert.SerialNumber.Text(16))
	require.Equal(t, http.StatusOK, status)
	cur, err := a.s.CRL()
	require.NoError(t, err)

	got, err := a.s.PushCRL(old)
	require.NoError(t, err)
	assert.Equal(t, cur, got, "the older list is not taken and the answer is the list held")
	still, err := a.s.CRL()
	require.NoError(t, err)
	assert.Equal(t, cur, still)
	require.ErrorIs(t, admit(t, a, a.s, op), identity.ErrRevoked, "a replayed older list does not un-revoke")
}

func TestCRLSync_AListTheRevocationKeyDidNotSignIsRefused(t *testing.T) {
	a := newEnv(t)
	other := newEnv(t)
	foreign, err := other.s.CRL()
	require.NoError(t, err)
	before, err := a.s.CRL()
	require.NoError(t, err)

	_, err = a.s.PushCRL(foreign)
	require.ErrorIs(t, err, identity.ErrCRLSignature)
	_, err = a.s.PushCRL([]byte("not a list"))
	require.Error(t, err)

	status, _ := a.do(http.MethodPost, "/v1/crl", foreign)
	assert.Equal(t, http.StatusForbidden, status)
	after, err := a.s.CRL()
	require.NoError(t, err)
	assert.Equal(t, before, after)

	status, body := a.do(http.MethodPost, "/v1/crl", before)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, before, body, "the answer is the list held")
}

func TestRevoke_RootsCertificateIsRevokedByRootAlone(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	rootSerial := e.rootID.cert.SerialNumber.Text(16)

	status, body := e.revoke(op, rootSerial)
	assert.Equal(t, http.StatusForbidden, status, "an operator may not revoke root's certificate: %s", body)
	got, _ := serialsOf(t, e, e.s)
	assert.Empty(t, got)

	for name, s := range map[string]*signer.Signer{"reopened": openAt(t, e.dir), "replica": replica(t, e)} {
		_, err := s.Revoke(e.signed(op, identity.ActionRevoke, identity.RevokeRequest{Serial: rootSerial}))
		require.ErrorIs(t, err, signer.ErrForbidden, name+": the role of a serial is kept in the directory and in a bundle")
	}

	status, _ = e.revoke(op, op.cert.SerialNumber.Text(16))
	assert.Equal(t, http.StatusOK, status, "control: an operator may revoke an operator's serial")
	status, body = e.revoke(e.rootID, rootSerial)
	assert.Equal(t, http.StatusOK, status, "root may revoke its own: %s", body)
	got, _ = serialsOf(t, e, e.s)
	assert.Contains(t, got, rootSerial)
}

func TestIssue_TheRoleOfEachSerialIsKeptInTheDirectoryOwnerOnly(t *testing.T) {
	e := newEnv(t)
	reader := e.issue(identity.RoleReader, "bob")
	path := filepath.Join(e.dir, "issued.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var roles map[string]string
	require.NoError(t, json.Unmarshal(data, &roles))
	assert.Equal(t, "root", roles[e.rootID.cert.SerialNumber.Text(16)])
	assert.Equal(t, string(identity.RoleReader), roles[reader.cert.SerialNumber.Text(16)])
}
