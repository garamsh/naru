package vip

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"
)

// Etcd names the port of etcd's client listener and the files a client of it presents and trusts.
type Etcd struct {
	Port, CAFile, CertFile, KeyFile string
}

// API names the API server's port, the CA it is trusted by, and Name, an address its serving
// certificate always names, which a member's own address need not be.
type API struct {
	Port, Name, CAFile string
}

// LiveProbe reports a member as alive when its API server answers an HTTP request over TLS and its
// etcd member answers /health over TLS with the client pair: etcd answers it only while it serves a
// quorum read, and the API server of a Node whose member does not is failing its requests, so that
// Node must not hold the address. A probe that completes its handshake leaves nothing for the API
// server to log. The CA and the pair are read on every probe, since init renews them.
func LiveProbe(api API, etcd Etcd, timeout time.Duration) Prober {
	return func(ctx context.Context, addr netip.Addr) bool {
		return api.answers(ctx, addr, timeout) && etcd.healthy(ctx, addr, timeout)
	}
}

// answers reports whether the API server at addr gives any HTTP response, whatever its status.
func (a API) answers(ctx context.Context, addr netip.Addr, timeout time.Duration) bool {
	pool, err := trust(a.CAFile)
	if err != nil {
		return false
	}
	res, err := get(ctx, addr, a.Port, "/", timeout, &tls.Config{RootCAs: pool, ServerName: a.Name, MinVersion: tls.VersionTLS12})
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return true
}

func (e Etcd) healthy(ctx context.Context, addr netip.Addr, timeout time.Duration) bool {
	pair, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
	if err != nil {
		return false
	}
	pool, err := trust(e.CAFile)
	if err != nil {
		return false
	}
	res, err := get(ctx, addr, e.Port, "/health", timeout, &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// trust is the pool of the CA in file.
func trust(file string) (*x509.CertPool, error) {
	ca, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New(file + " holds no certificate")
	}
	return pool, nil
}

// get requests path from addr's port over a connection of its own.
func get(ctx context.Context, addr netip.Addr, port, path string, timeout time.Duration, cfg *tls.Config) (*http.Response, error) {
	client := http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: cfg}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(addr.String(), port)+path, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}
