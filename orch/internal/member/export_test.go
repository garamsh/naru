package member

import "go.etcd.io/etcd/server/v3/embed"

// EtcdConfig is the configuration the member's embedded etcd runs with.
func (m *Member) EtcdConfig() embed.Config { return m.etcd.Config() }
