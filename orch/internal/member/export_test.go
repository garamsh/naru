package member

import (
	"crypto/tls"

	"go.etcd.io/etcd/server/v3/embed"
)

// EtcdConfig is the configuration the member's embedded etcd runs with.
func (m *Member) EtcdConfig() embed.Config { return m.etcd.Config() }

// ClientTLS is the TLS configuration of orch's own client of its member.
func ClientTLS(c Config) (*tls.Config, error) { return clientTLS(c) }
