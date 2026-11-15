//go:build qemu_real

package tests_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// hostdStatus is the status of GET /v1/microvms on a host's API from a client holding the certificate and key
// in the files, or the error of the exchange: a TLS 1.3 server that refuses a client's certificate shows it on
// the first read, so the request is what tells.
func hostdStatus(addr, certFile, keyFile string) (int, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return 0, err
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true}, //nolint:gosec // a test's request to a loopback forward; the server's certificate is not what is under test
		DisableKeepAlives: true,
	}}
	resp, err := c.Get("https://" + addr + "/v1/microvms") //nolint:noctx // bounded by the client's timeout
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// hostCertSerial is the serial, in hex, of the certificate naru-hostd at addr presents.
func hostCertSerial(t *testing.T, addr string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // only the presented certificate is read
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Text(16)
}

// The signed revocation list is checked on every naru TLS peer (ADR 0062): naru-hostd refuses the client
// certificate of an orch that was revoked, and orch stops calling a host whose certificate was revoked.
func TestFleet_ARevokedOrchCertificateIsRefusedByAHostAndARevokedHostIsNotCalledByOrch(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h).Addr
	orchCert, orchKey := filepath.Join(f.dir, orchName(1)+".crt"), f.keys[1]

	// Before: orch reaches the host, and the host serves orch's certificate.
	lastSeen := func() time.Time {
		hs := f.hostList(1)
		if len(hs) == 0 {
			return time.Time{}
		}
		return hs[0].LastSeen
	}
	eventually(t, "orch hears from the host", func() bool { return !lastSeen().IsZero() })
	code, err := hostdStatus(api, orchCert, orchKey)
	require.NoError(t, err, "the host serves orch's certificate before it is revoked")
	require.Equal(t, http.StatusOK, code)

	// Root revokes orch's certificate: the host, which reads the list from orch, refuses it once the list reaches it.
	// The list carries the revocation within its TTL; no restart of the host is needed.
	serial := hostCertSerial(t, f.forward(1, orchPort))
	f.must(nrun("operator", "revoke", "--admin", f.root.file, "--passphrase-file", f.pass, "--serial", serial, "--config", f.c.API.Admin), "revoke orch's certificate")
	eventually(t, "the host refuses the revoked orch certificate", func() bool {
		_, err := hostdStatus(api, orchCert, orchKey)
		return err != nil
	})
}

// A host whose certificate is revoked is not called by orch: its LastSeen stops advancing.
func TestFleet_OrchStopsCallingAHostWhoseCertificateWasRevoked(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	lastSeen := func() time.Time {
		hs := f.hostList(1)
		if len(hs) == 0 {
			return time.Time{}
		}
		return hs[0].LastSeen
	}
	var before time.Time
	eventually(t, "orch hears from the host again and again", func() bool {
		first := lastSeen()
		time.Sleep(2 * time.Second)
		before = lastSeen()
		return !first.IsZero() && before.After(first)
	})

	f.must(nrun("operator", "revoke", "--admin", f.root.file, "--passphrase-file", f.pass, "--serial", hostCertSerial(t, f.hostOfNode(h).Addr), "--config", f.c.API.Admin), "revoke the host's certificate")

	// Orch holds the list in its signer; its next call to the host is refused before the TLS handshake finishes.
	eventually(t, "orch stops hearing from the host", func() bool {
		a := lastSeen()
		time.Sleep(10 * time.Second)
		return lastSeen().Equal(a)
	})
}
