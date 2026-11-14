package config

import (
	"fmt"
	"slices"
	"strconv"
)

// named returns the services spec.files may name in restart: those of spec.services and the ones
// the sections derive.
func named(spec Spec) []Service {
	out := slices.Clone(spec.Services)
	if spec.Signer != nil {
		out = append(out, Service{Name: SignerService})
	}
	if spec.Orch != nil {
		out = append(out, Service{Name: OrchService})
	}
	return out
}

// serviceRuntime validates the user, the directories and the setup of the service at field.
func (v *validation) serviceRuntime(field string, s Service) {
	if u := s.User; u != nil && (u.UID == 0 || u.GID == 0 || slices.Contains(u.Groups, 0)) {
		v.fail(field+".user", "must name a user and a group other than root's 0; a service with no user runs as root")
	}
	for i, d := range s.Dirs {
		f := fmt.Sprintf("%s.dirs[%d]", field, i)
		if !isCleanAbs(d.Path) || d.Path == "/" {
			v.fail(f+".path", "must be a clean absolute path other than /, got %q", d.Path)
		}
		if d.Mode != "" {
			if m, err := strconv.ParseUint(d.Mode, 8, 32); err != nil || m > 0o777 {
				v.fail(f+".mode", "must be octal permission bits up to 0777, got %q", d.Mode)
			}
		}
	}
	for i, st := range s.Setup {
		f := fmt.Sprintf("%s.setup[%d]", field, i)
		if len(st.Args) == 0 {
			v.fail(f+".args", "must give the arguments of the service's binary")
		}
		for j, u := range st.Unless {
			if !isCleanAbs(u) {
				v.fail(fmt.Sprintf("%s.unless[%d]", f, j), "must be a clean absolute path, got %q", u)
			}
		}
	}
}
