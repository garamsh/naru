package vip

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// LiveProbe reports a member as alive when its API server accepts a connection and its etcd member
// answers /health over TLS with the client pair: etcd answers it only while it serves a quorum read,
// and the API server of a Node whose member does not is failing its requests, so that Node must not
// hold the address. The pair is read on every probe, since init renews it.
func LiveProbe(apiPort string, etcd Etcd, timeout time.Duration) Prober {
	dial := DialProbe(apiPort, timeout)
	return func(ctx context.Context, addr netip.Addr) bool {
		return dial(ctx, addr) && etcd.healthy(ctx, addr, timeout)
	}
}

func (e Etcd) healthy(ctx context.Context, addr netip.Addr, timeout time.Duration) bool {
	pair, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
	if err != nil {
		return false
	}
	ca, err := os.ReadFile(e.CAFile)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return false
	}
	client := http.Client{Timeout: timeout, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(addr.String(), e.Port)+"/health", nil)
	if err != nil {
		return false
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}
