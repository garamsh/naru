package config

import (
	"net/netip"
	"path"
)

func (v *validation) api(a *API) {
	if a == nil {
		return
	}
	if ap, err := netip.ParseAddrPort(a.ListenAddr()); err != nil || ap.Port() == 0 {
		v.fail("spec.api.listen", "must be an IP address and a port, such as %s, got %q", DefaultAPIListen, a.ListenAddr())
	}
	if ca, err := parseCertificate(a.ClientCA); err != nil {
		v.fail("spec.api.clientCA", "%v", err)
	} else if !ca.IsCA {
		v.fail("spec.api.clientCA", "is not a certificate authority")
	}
	cert, err := parseCertificate(a.Certificate)
	if err != nil {
		v.fail("spec.api.certificate", "%v", err)
	}
	if a.Key != "" && a.KeyFile != "" {
		v.fail("spec.api.keyFile", "must not be set beside spec.api.key: give the key or the path of its file, not both")
		return
	}
	if a.KeyFile != "" {
		if !path.IsAbs(a.KeyFile) || path.Clean(a.KeyFile) != a.KeyFile {
			v.fail("spec.api.keyFile", "must be a clean absolute path, got %q", a.KeyFile)
		}
		return
	}
	key, err := parseKey(a.Key)
	switch {
	case err != nil:
		v.fail("spec.api.key", "%v", err)
	case cert != nil && !publicKeysEqual(key.Public(), cert.PublicKey):
		v.fail("spec.api.key", "does not match spec.api.certificate")
	}
}
