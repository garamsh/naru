package api

import (
	"crypto/x509/pkix"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
)

// asUnits is a client whose certificate carries the given OrganizationalUnits and Organizations,
// as the signer's certificates carry a role in the OrganizationalUnit.
func (e *env) asUnits(t *testing.T, units, orgs []string) *http.Client {
	t.Helper()
	pair, _, _ := e.ca.issueSubject(t, pkix.Name{CommonName: "orch-1", OrganizationalUnit: units, Organization: orgs}, false)
	return e.client(pair)
}

func TestRoles_OrchsCertificateMayDoWhatOrchDoes(t *testing.T) {
	e := start(t)
	orch := e.asUnits(t, []string{string(identity.RoleOrch)}, nil)

	steps := []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/v1/host", nil, 200},
		{"POST", "/v1/microvms", createBody(), 201},
		{"POST", "/v1/microvms/web/stop", nil, 200},
		{"POST", "/v1/microvms/web/start", nil, 200},
		{"PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"}, 200},
		{"PUT", "/v1/peers", hostapi.PeersRequest{}, 200},
		{"GET", "/v1/tunnel", nil, 200},
		{"DELETE", "/v1/networks/a", nil, 204},
		{"DELETE", "/v1/microvms/web", nil, 204},
	}
	for _, s := range steps {
		code, b := e.do(t, orch, s.method, s.path, s.body)
		assert.Equal(t, s.want, code, "%s %s: %s", s.method, s.path, b)
	}
}

func TestRoles_OnlyTheOrchUnitGrantsTheOrchRole(t *testing.T) {
	e := start(t)
	// The control: orch's role writes.
	code, _ := e.do(t, e.asUnits(t, []string{string(identity.RoleOrch)}, nil), "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)

	for name, units := range map[string][]string{
		"host":         {string(identity.RoleHost)},
		"operator":     {string(identity.RoleOperator)},
		"reader":       {string(identity.RoleReader)},
		"orch and one": {string(identity.RoleOrch), string(identity.RoleOperator)},
		"unknown":      {"naru:host-provisioner"},
	} {
		c := e.asUnits(t, units, nil)
		code, _ := e.do(t, c, "GET", "/v1/host", nil)
		assert.Equal(t, 403, code, "%s may not read", name)
		code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
		assert.Equal(t, 403, code, "%s may not write", name)
	}
	assert.Len(t, e.rt.machines, 1, "a refused request reached mvmd")
}

func TestRoles_OrchsUnitDoesNotRaiseAReader(t *testing.T) {
	e := start(t)
	// A reader's Organization with no unit still reads and does not write.
	reader := e.asUnits(t, nil, []string{ReaderOrganization})
	code, _ := e.do(t, reader, "GET", "/v1/host", nil)
	assert.Equal(t, 200, code)
	code, _ = e.do(t, reader, "POST", "/v1/microvms", createBody())
	assert.Equal(t, 403, code)
}
