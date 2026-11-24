package network

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/garamsh/naru/lib/atomicfile"
)

// Tunnel is the host's end of the WireGuard tunnel: an interface the host made, with the key it
// generated. Manager sets its peers; the host API reports its public key and port.
type Tunnel interface {
	// PublicKey is the public half of the tunnel's key, in base64. The private half never leaves the host.
	PublicKey() string
	// Port is the UDP port the tunnel listens on.
	Port() int
	// Set makes mesh the interface's one address (none when mesh is the zero value) and peers its
	// whole peer list, and routes each peer's prefixes and mesh address to it and no other prefix.
	Set(mesh netip.Addr, peers []Peer) error
}

// TunnelInfo is what the host API reports of a tunnel. Mesh is the zero value until orch declares
// the host's mesh address.
type TunnelInfo struct {
	PublicKey string
	Port      int
	Mesh      netip.Addr
}

// LoadKey returns the private key in the file at path, which is made with a new key when it does
// not exist. The file is written with mode 0600 in a directory of mode 0700, and one whose mode
// allows group or others any access is refused. The key is never in an error or a log.
func LoadKey(path string) (wgtypes.Key, error) {
	st, err := os.Stat(path)
	switch {
	case err == nil:
		if st.Mode().Perm()&0o077 != 0 {
			return wgtypes.Key{}, fmt.Errorf("the tunnel key %s must have mode 0600, has %v", path, st.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return wgtypes.Key{}, err
		}
		key, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return wgtypes.Key{}, fmt.Errorf("the tunnel key %s is not a WireGuard key", path)
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return wgtypes.Key{}, err
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, err
	}
	if err := writeFile(path, []byte(base64.StdEncoding.EncodeToString(key[:])+"\n")); err != nil {
		return wgtypes.Key{}, err
	}
	return key, nil
}

// writeFile writes data to path with mode 0600 through a temporary file and a rename, so a reader
// sees all of the old contents or all of the new ones.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}
