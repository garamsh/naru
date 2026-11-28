package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/garamsh/naru/lib/wire"
)

// What init derives for the join form, the configuration of a host that has not joined an orch yet
// (docs/architecture/adr/0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md).
const (
	// HostdKeyFile is the host's private key, which hostd creates when it joins.
	HostdKeyFile = wire.DataMount + "/hostd/host.key"
	// JoinConfigFile is where hostd writes the configuration it brings back from orch.
	JoinConfigFile = wire.DataMount + "/join/config.yaml"
)

// Join is the join form: the one section of a configuration that has no other. A host that boots
// from it runs hostd in join mode, which enrolls with the orch and writes back the host's full
// configuration.
type Join struct {
	Orch JoinOrch `yaml:"orch"`
	// EnrollKey is an enrollment key orch issued, in the form naru-ek-<64 hexadecimal digits>.
	EnrollKey string `yaml:"enrollKey"`
}

// JoinOrch is the orch the host joins.
type JoinOrch struct {
	// URL is the https URL of orch's API, with a host and a port.
	URL string `yaml:"url"`
	// CASHA256 is the hex SHA-256 of the public key (SubjectPublicKeyInfo) of the authority orch's
	// certificate chains to, which a re-signing keeps; until v0.1.0-rc.1 the SHA-256 of its certificate
	// is taken too (ADR 0122).
	CASHA256 string `yaml:"caSHA256"`
}

// Restart policies of a service.
const (
	// RestartAlways restarts the service after every exit. It is the policy when none is given.
	RestartAlways = "always"
	// RestartOnFailure restarts the service after an exit with a non-zero status or a signal, and
	// leaves it stopped after an exit with status 0.
	RestartOnFailure = "on-failure"
)

// joinDataDisk is the data disk of a join form: the disk labelled wire.DataLabel, or else the first
// blank disk that is not the boot disk, which init formats.
func joinDataDisk() DataDisk {
	return DataDisk{Label: wire.DataLabel, MountPoint: wire.DataMount, FormatBlank: true}
}

func joinService(j Join) Service {
	return Service{
		Name:   HostdService,
		Binary: hostdBinary,
		Args: []string{
			"--join",
			"--orch-url=" + j.Orch.URL,
			"--orch-ca-sha256=" + j.Orch.CASHA256,
			"--enroll-key=" + HostdEnrollKeyFile,
			"--host-key=" + HostdKeyFile,
			"--join-config=" + JoinConfigFile,
		},
		RestartPolicy: RestartOnFailure,
		Dirs: []Dir{
			{Path: wire.DataMount + "/hostd", Mode: "0700"},
			{Path: wire.DataMount + "/join", Mode: "0700"},
		},
	}
}

// joinKeyFile is the file init writes the enrollment key to. hostd runs as root, so root owns it.
func joinKeyFile(j Join) File {
	return File{Path: HostdEnrollKeyFile, Mode: "0600", Content: j.EnrollKey}
}

// validateJoin checks a configuration that carries spec.join: the form itself, and that spec holds
// nothing else but what init derived from the form, which a stored configuration carries.
func validateJoin(cfg NodeConfig) error {
	var v validation
	if cfg.APIVersion != APIVersion {
		v.fail("apiVersion", "must be %q, got %q", APIVersion, cfg.APIVersion)
	}
	if cfg.Kind != Kind {
		v.fail("kind", "must be %q, got %q", Kind, cfg.Kind)
	}
	if n := cfg.Metadata.Name; n != "" && !nameRE.MatchString(n) {
		v.fail("metadata.name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", n)
	}
	j := cfg.Spec.Join
	if !httpsURL(j.Orch.URL) {
		v.fail("spec.join.orch.url", "must be an https URL with a host and a port, got %q", j.Orch.URL)
	}
	if d, err := hex.DecodeString(j.Orch.CASHA256); err != nil || len(d) != sha256.Size {
		v.fail("spec.join.orch.caSHA256", "must be the SHA-256 of the authority's public key (or, until v0.1.0-rc.1, of its certificate), 64 hexadecimal digits")
	}
	if !wire.IsEnrollKey(j.EnrollKey) {
		v.fail("spec.join.enrollKey", "must be an enrollment key: %s followed by 64 lowercase hexadecimal digits", wire.EnrollKeyPrefix)
	}
	for _, f := range otherFields(cfg.Spec) {
		v.fail("spec."+f, "must not be set beside spec.join: the join form holds no other field")
	}
	return errors.Join(v.errs...)
}

// otherFields names the fields of s other than join, in the order Spec lists them. What init
// derives from the form (the hostd service, the file of the enrollment key and the data disk) is
// not one of them, so that a stored form parses again.
func otherFields(s Spec) []string {
	derived := joinService(*s.Join)
	key := joinKeyFile(*s.Join)
	disk := s.DataDisk
	disk.FirstBlank = false
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"services", slices.ContainsFunc(s.Services, func(svc Service) bool { return !svc.Equal(derived) })},
		{"controlPlane", s.ControlPlane},
		{"dataDisk", disk != DataDisk{} && disk != joinDataDisk()},
		{"network", len(s.Network.Forwarding) > 0 || len(s.Network.Routes) > 0},
		{"files", slices.ContainsFunc(s.Files, func(f File) bool {
			return f.Path != key.Path || f.Mode != key.Mode || f.Content != key.Content || len(f.Restart) > 0 || f.Owner != nil
		})},
		{"pki", len(s.PKI.Authorities) > 0 || len(s.PKI.Certificates) > 0},
		{"time", s.Time != nil},
		{"api", s.API != nil},
		{"registries", len(s.Registries) > 0},
		{"aws", s.AWS != nil},
		{"storage", len(s.Storage) > 0},
		{"host", s.Host != nil},
		{"orch", s.Orch != nil},
		{"signer", s.Signer != nil},
		{"extensions", len(s.Extensions) > 0},
		{"metrics", s.Metrics != nil},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// deriveJoin gives a join form what init runs for it: the data disk, the file that holds the
// enrollment key and hostd in join mode.
func deriveJoin(cfg NodeConfig) NodeConfig {
	j := *cfg.Spec.Join
	cfg.Spec.DataDisk = joinDataDisk()
	cfg.Spec.DataDisk.FirstBlank = true
	cfg.Spec.Files = []File{joinKeyFile(j)}
	cfg.Spec.Services = []Service{joinService(j)}
	return cfg
}
