package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/enroll"
)

// pairStub is the host's pair as the API sees it: it records what it was given and answers err.
type pairStub struct {
	cert, key string
	err       error
}

func (p *pairStub) Rotate(_ context.Context, cert, key []byte) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	p.cert, p.key = string(cert), string(key)
	return "2a", nil
}

func TestIdentity_PutReplacesThePairAndAnswersItsSerial(t *testing.T) {
	stub := &pairStub{}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.Identity = stub })
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/identity", map[string]string{"certificate": "CRT", "key": "KEY"})
	require.Equal(t, 200, code, string(b))
	var got identityAnswer
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "2a", got.Serial)
	assert.Equal(t, "CRT", stub.cert)
	assert.Equal(t, "KEY", stub.key)
}

func TestIdentity_ARefusalIsAStatusAndAReaderMayNotReplaceThePair(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{enroll.ErrNotEnrolled, 409},
		{fmt.Errorf("%w: wrong host", enroll.ErrBadPair), 422},
		{fmt.Errorf("%w: revoked", enroll.ErrOrchRefused), 502},
		{fmt.Errorf("disk full"), 500},
	} {
		stub := &pairStub{err: c.err}
		e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.Identity = stub })
		code, _ := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/identity", map[string]string{"certificate": "C", "key": "K"})
		assert.Equal(t, c.want, code, c.err.Error())
	}
	stub := &pairStub{}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.Identity = stub })
	code, _ := e.do(t, e.as(t, ReaderOrganization), "PUT", "/v1/identity", map[string]string{"certificate": "C", "key": "K"})
	assert.Equal(t, 403, code)
	assert.Empty(t, stub.cert, "a refused request reached the pair")
	code, _ = e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/identity", map[string]string{"certificate": "C", "other": "K"})
	assert.Equal(t, 400, code, "a body with a field the API does not name")
}

func TestIdentity_AHostWithNoOrchAnswers409(t *testing.T) {
	e := start(t)
	code, _ := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/identity", map[string]string{"certificate": "C", "key": "K"})
	assert.Equal(t, 409, code)
}
