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

// Aliases checks that an alias is unique across callers, follows the zone's rules inside it, needs
// a granted domain and its own certificate outside it, never answers its key, and goes with its
// Machine.
func Aliases(t *testing.T, c Config) {
	c = c.check(t)
	require.NotEmpty(t, c.AliasDomain, "Config.AliasDomain")
	ctx := t.Context()
	name, theirs := unique("aliases"), unique("aliases-theirs")
	create(t, c.Caller, name, c.Spec)
	create(t, c.Other, theirs, c.Spec)
	put := func(cl *machineapi.Client, host string, r machineapi.AliasRequest) (machineapi.Alias, bool, error) {
		t.Cleanup(func() { _ = cl.DeleteAlias(context.Background(), host) })
		return cl.PutAlias(ctx, host, r)
	}

	host := unique("alias") + "." + c.Zone
	a, created, err := put(c.Caller, host, machineapi.AliasRequest{Machine: name})
	require.NoError(t, err, "an alias in the zone")
	assert.True(t, created)
	assert.Equal(t, machineapi.Alias{Hostname: host, Machine: name, Service: c.Spec.Services[0].Name}, a, "of the default service, with no certificate")
	_, created, err = put(c.Caller, host, machineapi.AliasRequest{Machine: name})
	require.NoError(t, err, "the same alias again")
	assert.False(t, created, "answers 200")
	_, _, err = put(c.Other, host, machineapi.AliasRequest{Machine: theirs})
	refused(t, err, http.StatusConflict, machineapi.CodeAliasTaken, "another caller's hostname")
	_, created, err = put(c.Other, unique("alias")+"."+c.Zone, machineapi.AliasRequest{Machine: theirs})
	require.NoError(t, err, "the other caller's own hostname")
	assert.True(t, created)

	for what, h := range map[string]string{
		"a Machine's stable name": "0123456789abcdef." + c.Zone,
		"a service's form":        "web--" + unique("alias") + "." + c.Zone,
		"two labels":              "a." + unique("alias") + "." + c.Zone,
		"the zone itself":         c.Zone,
		"upper case":              strings.ToUpper(unique("alias")) + "." + c.Zone,
	} {
		_, _, err = put(c.Caller, h, machineapi.AliasRequest{Machine: name})
		refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "an alias of "+what)
	}
	_, _, err = put(c.Caller, unique("alias")+"."+c.Zone, machineapi.AliasRequest{Machine: name, Service: "conformance-absent"})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a service the Machine has not")
	_, _, err = put(c.Caller, unique("alias")+"."+c.Zone, machineapi.AliasRequest{Machine: unique("absent")})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "an alias of no Machine")

	outside := unique("alias") + "." + c.AliasDomain
	cert, key := certificate(t, outside)
	_, _, err = put(c.Caller, unique("alias")+".conformance.invalid", machineapi.AliasRequest{Machine: name, Certificate: cert, Key: key})
	refused(t, err, http.StatusForbidden, machineapi.CodeAliasDomainNotGranted, "a domain granted to no caller")
	_, _, err = put(c.Other, outside, machineapi.AliasRequest{Machine: theirs, Certificate: cert, Key: key})
	refused(t, err, http.StatusForbidden, machineapi.CodeAliasDomainNotGranted, "a domain granted to another caller")
	_, _, err = put(c.Caller, outside, machineapi.AliasRequest{Machine: name})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "an alias outside the zone without a certificate")
	wrong, wrongKey := certificate(t, "other."+c.AliasDomain)
	_, _, err = put(c.Caller, outside, machineapi.AliasRequest{Machine: name, Certificate: wrong, Key: wrongKey})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a certificate of another name")
	zoneCert, zoneKey := certificate(t, host)
	_, _, err = put(c.Caller, unique("alias")+"."+c.Zone, machineapi.AliasRequest{Machine: name, Certificate: zoneCert, Key: zoneKey})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a certificate for an alias in the zone")
	a, created, err = put(c.Caller, outside, machineapi.AliasRequest{Machine: name, Certificate: cert, Key: key})
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
	_, created, err = put(c.Other, host, machineapi.AliasRequest{Machine: theirs})
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
