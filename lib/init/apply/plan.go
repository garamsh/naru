package apply

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/garamsh/naru/lib/init/config"
)

// redacted is what GET /v1/config shows in place of private material.
const redacted = "REDACTED"

// step is an action and what carries it out.
type step struct {
	action Action
	do     func() error
}

// refuse returns an *InvalidError when next carries REDACTED in a place that
// holds private material it did not hold before, changes a field no call may
// change, or, in no-reboot mode, changes a field that needs a reboot. Fields
// are compared with the configuration that runs.
func (e *Engine) refuse(next config.NodeConfig, mode Mode) error {
	var placeholders, never, reboot []string
	stored, running := e.stored.Config, e.running

	if a := next.Spec.API; a != nil && a.Key == redacted && (stored.Spec.API == nil || stored.Spec.API.Key != redacted) {
		placeholders = append(placeholders, "spec.api.key")
	}
	for i, a := range next.Spec.PKI.Authorities {
		if old, ok := stored.Spec.PKI.Authority(a.Name); a.Key == redacted && (!ok || old.Key != redacted) {
			placeholders = append(placeholders, fmt.Sprintf("spec.pki.authorities[%d].key", i))
		}
	}
	for i, f := range next.Spec.Files {
		if f.Perm()&0o044 != 0 || f.Content != redacted {
			continue
		}
		if old, ok := fileAt(stored.Spec.Files, f.Path); !ok || old.Content != redacted {
			placeholders = append(placeholders, fmt.Sprintf("spec.files[%d].content", i))
		}
	}
	for i, r := range next.Spec.Registries {
		if r.Credential == nil || r.Credential.Password != redacted {
			continue
		}
		if old, ok := stored.Spec.Registry(r.Host); !ok || old.Credential == nil || old.Credential.Password != redacted {
			placeholders = append(placeholders, fmt.Sprintf("spec.registries[%d].credential.password", i))
		}
	}
	if len(placeholders) > 0 {
		return &InvalidError{Fields: placeholders, Message: fmt.Sprintf("carries %s, which stands for private material the API never returns; give the material or leave the field out of the patch", redacted)}
	}

	if next.APIVersion != running.APIVersion {
		never = append(never, "apiVersion")
	}
	if next.Kind != running.Kind {
		never = append(never, "kind")
	}
	if next.Metadata != running.Metadata {
		never = append(never, "metadata.name")
	}
	if next.Spec.DataDisk != running.Spec.DataDisk {
		never = append(never, "spec.dataDisk")
	}
	if len(never) > 0 {
		return &InvalidError{Fields: never, Message: "no call may change these fields"}
	}

	if mode != NoReboot {
		return nil
	}
	if next.Spec.ControlPlane != running.Spec.ControlPlane {
		reboot = append(reboot, "spec.controlPlane")
	}
	if !slices.Equal(next.Spec.Network.Forwarding, running.Spec.Network.Forwarding) {
		reboot = append(reboot, "spec.network.forwarding")
	}
	if !slices.Equal(next.Spec.PKI.Authorities, running.Spec.PKI.Authorities) {
		reboot = append(reboot, "spec.pki.authorities")
	}
	switch na, ra := next.Spec.API, running.Spec.API; {
	case (na == nil) != (ra == nil):
		reboot = append(reboot, "spec.api")
	case na != nil && na.ListenAddr() != ra.ListenAddr():
		reboot = append(reboot, "spec.api.listen")
	}
	if next.Spec.MetricsAddr() != running.Spec.MetricsAddr() {
		reboot = append(reboot, "spec.metrics")
	}
	if !slices.Equal(next.Spec.Storage, running.Spec.Storage) {
		reboot = append(reboot, "spec.storage")
	}
	if !slices.Equal(next.Spec.Extensions, running.Spec.Extensions) {
		reboot = append(reboot, "spec.extensions")
	}
	if len(reboot) > 0 {
		return &InvalidError{Fields: reboot, Message: `these fields need a reboot; use mode "staged" or "reboot"`}
	}
	return nil
}

// plan returns the live steps that take the running configuration to next, in
// the order they run: the clock's servers change first, since the certificates
// issued next depend on the clock; routes are deleted and added; files and
// certificates are written, then the listener reloads, then services stop,
// start or restart, and last the services a written file or certificate names
// restart, each once and only if the apply did not start or restart it
// already.
func (e *Engine) plan(next config.NodeConfig) []step {
	running := e.running
	var steps []step
	var named []string
	name := func(restart []string) {
		for _, n := range restart {
			if !slices.Contains(named, n) {
				named = append(named, n)
			}
		}
	}

	switch nt, rt := next.Spec.Time.Addrs(), running.Spec.Time.Addrs(); {
	case len(nt) == 0 && len(rt) > 0:
		steps = append(steps, step{Action{"stop-time-sync", "spec.time"}, func() error { e.deps.Time.ConfigureTime(nil); return nil }})
	case len(nt) > 0 && !slices.Equal(nt, rt):
		steps = append(steps, step{Action{"sync-time", "spec.time"}, func() error { e.deps.Time.ConfigureTime(nt); return nil }})
	}
	for _, old := range running.Spec.Network.Routes {
		if _, ok := routeAt(next.Spec.Network.Routes, old.To); ok {
			continue // a changed next hop is replaced by the add below
		}
		steps = append(steps, step{Action{"delete-route", old.To.String()}, func() error { return e.deps.Routes.DeleteRoute(old) }})
	}
	for _, r := range next.Spec.Network.Routes {
		if old, ok := routeAt(running.Spec.Network.Routes, r.To); ok && old.Via == r.Via {
			continue
		}
		steps = append(steps, step{Action{"add-route", r.To.String()}, func() error { return e.deps.Routes.AddRoute(r) }})
	}
	for _, f := range next.Spec.Files {
		if old, ok := fileAt(running.Spec.Files, f.Path); ok && old.Perm() == f.Perm() && old.Content == f.Content && ownerEqual(old.Owner, f.Owner) {
			continue
		}
		steps = append(steps, step{Action{"write-file", f.Path}, func() error { return e.deps.Files.ReplaceFile(f) }})
		name(f.Restart)
	}
	if !registryFilesEqual(running.Spec, next.Spec) {
		steps = append(steps, step{Action{"write-registries", "spec.registries"}, func() error { return e.deps.Registries.WriteRegistries(next.Spec) }})
	}
	for _, c := range next.Spec.PKI.Certificates {
		if old, ok := certificateAt(running.Spec.PKI.Certificates, c.Name); ok && old.Equal(c) {
			continue
		}
		steps = append(steps, step{Action{"issue-certificate", c.Name}, func() error {
			return e.deps.Certificates.Reissue(next.Spec.PKI, []string{c.Name})
		}})
		name(c.Restart)
	}
	for _, old := range running.Spec.PKI.Certificates {
		if _, ok := certificateAt(next.Spec.PKI.Certificates, old.Name); ok {
			continue
		}
		// The issuer renews what its spec holds, so it is given the spec
		// without the deleted certificate; it signs nothing.
		steps = append(steps, step{Action{"forget-certificate", old.Name}, func() error {
			return e.deps.Certificates.Reissue(next.Spec.PKI, nil)
		}})
	}
	if na, ra := next.Spec.API, running.Spec.API; na != nil && ra != nil && *na != *ra {
		steps = append(steps, step{Action{"reload-api", "spec.api"}, func() error { return e.deps.API.Reload(*na) }})
	}

	touched := map[string]bool{}
	for _, old := range running.Spec.Services {
		if !running.Spec.Runs(old) {
			continue
		}
		if _, ok := serviceAt(next.Spec.Services, old.Name); ok && next.Spec.Runs(old) {
			continue
		}
		steps = append(steps, step{Action{"stop", old.Name}, func() error { e.deps.Services.Stop(old.Name); return nil }})
		touched[old.Name] = true
	}
	for _, svc := range next.Spec.Services {
		if !next.Spec.Runs(svc) {
			continue
		}
		old, ok := serviceAt(running.Spec.Services, svc.Name)
		switch {
		case !ok || !running.Spec.Runs(old):
			steps = append(steps, step{Action{"start", svc.Name}, func() error { return e.deps.Services.Start(svc) }})
		case !old.Equal(svc):
			steps = append(steps, step{Action{"restart", svc.Name}, func() error {
				e.deps.Services.Stop(svc.Name)
				return e.deps.Services.Start(svc)
			}})
		default:
			continue
		}
		touched[svc.Name] = true
	}
	for _, n := range named {
		svc, ok := serviceAt(next.Spec.Services, n)
		if touched[n] || !ok || !next.Spec.Runs(svc) {
			continue
		}
		steps = append(steps, step{Action{"restart", n}, func() error { e.deps.Services.Restart(n); return nil }})
	}
	return steps
}

// registryFilesEqual reports whether a and b give the same registry files: the same hosts,
// authorities and credentials.
func registryFilesEqual(a, b config.Spec) bool {
	if len(a.Registries) != len(b.Registries) {
		return false
	}
	for i, r := range a.Registries {
		o := b.Registries[i]
		if r.Host != o.Host || r.CA != o.CA || (r.Credential == nil) != (o.Credential == nil) || (r.Credential != nil && *r.Credential != *o.Credential) {
			return false
		}
	}
	return true
}

func ownerEqual(a, b *config.Owner) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func routeAt(routes []config.Route, to netip.Prefix) (config.Route, bool) {
	i := slices.IndexFunc(routes, func(r config.Route) bool { return r.To == to })
	if i < 0 {
		return config.Route{}, false
	}
	return routes[i], true
}

func fileAt(files []config.File, path string) (config.File, bool) {
	i := slices.IndexFunc(files, func(f config.File) bool { return f.Path == path })
	if i < 0 {
		return config.File{}, false
	}
	return files[i], true
}

func certificateAt(certs []config.Certificate, name string) (config.Certificate, bool) {
	i := slices.IndexFunc(certs, func(c config.Certificate) bool { return c.Name == name })
	if i < 0 {
		return config.Certificate{}, false
	}
	return certs[i], true
}

func serviceAt(services []config.Service, name string) (config.Service, bool) {
	i := slices.IndexFunc(services, func(s config.Service) bool { return s.Name == name })
	if i < 0 {
		return config.Service{}, false
	}
	return services[i], true
}
