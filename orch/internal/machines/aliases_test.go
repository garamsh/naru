package machines_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// served is spec with one HTTP service, which an alias names.
func served() machineapi.Spec {
	s := spec()
	s.Services = []machineapi.Service{{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP}}
	return s
}

// aliasDomain is the domain granted garam, outside the zone, where naru takes no alias.
const aliasDomain = "apps.example.test"

// aliases grants garam aliasDomain and returns a request for an alias of machine at host, with a
// certificate for host, the same pair for the same host, so a repeat is the same request.
func aliases(t *testing.T, r *rig) func(host, machine string) machineapi.AliasRequest {
	t.Helper()
	_, err := r.svc.PutCaller(t.Context(), machines.Caller{Name: "garam", AliasDomains: []string{aliasDomain}})
	require.NoError(t, err)
	pairs := map[string]machineapi.AliasRequest{}
	return func(host, machine string) machineapi.AliasRequest {
		if p, ok := pairs[host]; ok {
			p.Machine = machine
			return p
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		require.NoError(t, err)
		kder, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		p := machineapi.AliasRequest{
			Machine:     machine,
			Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			Key:         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})),
		}
		pairs[host] = p
		return p
	}
}

func TestPutAlias_RefusesAMachineWithNoServiceOrBeingDeleted(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	alias := aliases(t, r)
	r.create("bare", spec())
	_, _, err := r.svc.PutAlias(ctx, "garam", "bare."+aliasDomain, alias("bare."+aliasDomain, "bare"))
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "a Machine with no service has nothing to route to")

	r.create("web", served())
	_, created, err := r.svc.PutAlias(ctx, "garam", "web."+aliasDomain, alias("web."+aliasDomain, "web"))
	require.NoError(t, err, "control: a Machine with a service")
	assert.True(t, created)

	r.create("gone", served())
	_, err = r.svc.Delete(ctx, "garam", "gone")
	require.NoError(t, err)
	m, err := r.svc.Machine(ctx, "garam", "gone")
	require.NoError(t, err)
	require.Equal(t, machineapi.PhaseDeleting, m.Status.Phase, "the setup: the Machine is still being deleted")
	_, _, err = r.svc.PutAlias(ctx, "garam", "gone."+aliasDomain, alias("gone."+aliasDomain, "gone"))
	assert.ErrorIs(t, err, machines.ErrMachineNotFound, "an alias of a Machine being deleted would outlive it")
}

func TestPutAlias_RefusesAnAliasPastTheMachinesBound(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	alias := aliases(t, r)
	r.create("web", served())
	for i := range machines.MaxAliasesPerMachine {
		_, _, err := r.svc.PutAlias(ctx, "garam", fmt.Sprintf("a%d.%s", i, aliasDomain), alias(fmt.Sprintf("a%d.%s", i, aliasDomain), "web"))
		require.NoError(t, err, "alias %d, within the bound", i)
	}
	_, _, err := r.svc.PutAlias(ctx, "garam", "a0."+aliasDomain, alias("a0."+aliasDomain, "web"))
	require.NoError(t, err, "control: one the Machine holds, asked again, is not one more")
	_, _, err = r.svc.PutAlias(ctx, "garam", "over."+aliasDomain, alias("over."+aliasDomain, "web"))
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "one past the bound its delete removes in one transaction")
}
