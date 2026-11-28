package machineapitest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

// Aliases checks that an alias is unique across callers, is never in the zone, needs a granted
// domain and its own certificate, never answers its key, and goes with its Machine.
func Aliases(t *testing.T, c Config) {
	c = c.check(t)
	require.NotEmpty(t, c.AliasDomain, "Config.AliasDomain")
	require.NotEmpty(t, c.SharedAliasDomain, "Config.SharedAliasDomain")
	ctx := t.Context()
	name, theirs := unique("aliases"), unique("aliases-theirs")
	create(t, c.Caller, name, c.Spec)
	create(t, c.Other, theirs, c.Spec)
	// put publishes host with a certificate for it, the same pair for the same host, so a repeat is
	// the same request.
	pairs := map[string][2]string{}
	put := func(cl *machineapi.Client, host, machine, service string) (machineapi.Alias, bool, error) {
		t.Cleanup(func() { _ = cl.DeleteAlias(context.Background(), host) })
		p, ok := pairs[host]
		if !ok {
			p[0], p[1] = certificate(t, host)
			pairs[host] = p
		}
		return cl.PutAlias(ctx, host, machineapi.AliasRequest{Machine: machine, Service: service, Certificate: p[0], Key: p[1]})
	}

	host := unique("alias") + "." + c.SharedAliasDomain
	a, created, err := put(c.Caller, host, name, "")
	require.NoError(t, err, "an alias in a granted domain")
	assert.True(t, created)
	assert.Equal(t, []any{host, name, c.Spec.Services[0].Name, machineapi.Redacted}, []any{a.Hostname, a.Machine, a.Service, a.Key}, "of the default service")
	_, created, err = put(c.Caller, host, name, "")
	require.NoError(t, err, "the same alias again")
	assert.False(t, created, "answers 200")
	_, _, err = put(c.Other, host, theirs, "")
	refused(t, err, http.StatusConflict, machineapi.CodeAliasTaken, "another caller's hostname")
	_, created, err = put(c.Other, unique("alias")+"."+c.SharedAliasDomain, theirs, "")
	require.NoError(t, err, "the other caller's own hostname")
	assert.True(t, created)

	for what, h := range map[string]string{
		"a name in the zone":      unique("alias") + "." + c.Zone,
		"a Machine's stable name": machineapi.StableName("0123456789abcdef", "", c.Zone),
		"the zone itself":         c.Zone,
		"upper case":              strings.ToUpper(unique("alias")) + "." + c.SharedAliasDomain,
	} {
		_, _, err = put(c.Caller, h, name, "")
		refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "an alias of "+what)
	}
	_, _, err = put(c.Caller, unique("alias")+"."+c.SharedAliasDomain, name, "conformance-absent")
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a service the Machine has not")
	_, _, err = put(c.Caller, unique("alias")+"."+c.SharedAliasDomain, unique("absent"), "")
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "an alias of no Machine")

	outside := unique("alias") + "." + c.AliasDomain
	_, _, err = put(c.Caller, unique("alias")+".conformance.invalid", name, "")
	refused(t, err, http.StatusForbidden, machineapi.CodeAliasDomainNotGranted, "a domain granted to no caller")
	_, _, err = put(c.Other, outside, theirs, "")
	refused(t, err, http.StatusForbidden, machineapi.CodeAliasDomainNotGranted, "a domain granted to another caller")
	t.Cleanup(func() { _ = c.Caller.DeleteAlias(context.Background(), outside) })
	_, _, err = c.Caller.PutAlias(ctx, outside, machineapi.AliasRequest{Machine: name})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "an alias without a certificate")
	wrong, wrongKey := certificate(t, "other."+c.AliasDomain)
	_, _, err = c.Caller.PutAlias(ctx, outside, machineapi.AliasRequest{Machine: name, Certificate: wrong, Key: wrongKey})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a certificate of another name")
	a, created, err = put(c.Caller, outside, name, "")
	require.NoError(t, err, "an alias in the granted domain with its certificate")
	assert.True(t, created)
	got, err := c.Caller.Alias(ctx, outside)
	require.NoError(t, err)
	for what, al := range map[string]machineapi.Alias{"put": a, "read": got} {
		assert.Equal(t, machineapi.Redacted, al.Key, "%s never answers the key", what)
		require.NotNil(t, al.Certificate, "%s shows the certificate", what)
		assert.Equal(t, []string{outside}, al.Certificate.Names, "%s shows its names", what)
	}

	require.NoError(t, c.Caller.DeleteMachine(ctx, name))
	waitGone(t, c, c.Caller, name)
	for _, h := range []string{host, outside} {
		_, err = c.Caller.Alias(ctx, h)
		refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "an alias of a deleted Machine")
	}
	_, created, err = put(c.Other, host, theirs, "")
	require.NoError(t, err, "a hostname freed by its Machine's delete")
	assert.True(t, created)
}

// certificate returns a self-signed PEM certificate for host, valid now, and its PEM key.
func certificate(t *testing.T, host string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}))
}
