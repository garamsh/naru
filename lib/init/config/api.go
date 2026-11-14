package config

import "net/netip"

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
	key, err := parseKey(a.Key)
	switch {
	case err != nil:
		v.fail("spec.api.key", "%v", err)
	case cert != nil && !publicKeysEqual(key.Public(), cert.PublicKey):
		v.fail("spec.api.key", "does not match spec.api.certificate")
	}
}
