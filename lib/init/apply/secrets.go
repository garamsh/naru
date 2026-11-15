package apply

import (
	"fmt"
	"slices"

	"github.com/garamsh/naru/lib/init/config"
)

// Redacted is what GET /v1/config shows in place of private material.
const Redacted = "REDACTED"

// secret is one place a configuration holds private material.
type secret struct {
	// field names the place for a refusal, with the index of a list element.
	field string
	// id names the place across two configurations: a list element by its key, not its index.
	id    string
	value *string
}

// secretKinds is the one table of private material. Redact hides what each kind lists and refuse
// turns away a placeholder where none was stored, so a kind listed here is both.
var secretKinds = []struct {
	// own copies what a secret of this kind sits in, so that setting it leaves the caller's
	// configuration alone.
	own func(cfg *config.NodeConfig)
	// places lists the secrets of this kind that cfg holds, empty ones included.
	places func(cfg *config.NodeConfig) []secret
}{
	{
		own: func(cfg *config.NodeConfig) {
			if a := cfg.Spec.API; a != nil {
				c := *a
				cfg.Spec.API = &c
			}
		},
		places: func(cfg *config.NodeConfig) []secret {
			if a := cfg.Spec.API; a != nil {
				return []secret{{"spec.api.key", "spec.api.key", &a.Key}}
			}
			return nil
		},
	},
	{
		own: func(cfg *config.NodeConfig) { cfg.Spec.PKI.Authorities = slices.Clone(cfg.Spec.PKI.Authorities) },
		places: func(cfg *config.NodeConfig) []secret {
			var out []secret
			for i := range cfg.Spec.PKI.Authorities {
				a := &cfg.Spec.PKI.Authorities[i]
				out = append(out, secret{fmt.Sprintf("spec.pki.authorities[%d].key", i), "spec.pki.authorities/" + a.Name, &a.Key})
			}
			return out
		},
	},
	{
		own: func(cfg *config.NodeConfig) { cfg.Spec.Files = slices.Clone(cfg.Spec.Files) },
		places: func(cfg *config.NodeConfig) []secret {
			var out []secret
			for i := range cfg.Spec.Files {
				// A file group and other can read holds no private material.
				if f := &cfg.Spec.Files[i]; f.Perm()&0o044 == 0 {
					out = append(out, secret{fmt.Sprintf("spec.files[%d].content", i), "spec.files/" + f.Path, &f.Content})
				}
			}
			return out
		},
	},
	{
		own: func(cfg *config.NodeConfig) {
			cfg.Spec.Registries = slices.Clone(cfg.Spec.Registries)
			for i, r := range cfg.Spec.Registries {
				if r.Credential != nil {
					c := *r.Credential
					cfg.Spec.Registries[i].Credential = &c
				}
			}
		},
		places: func(cfg *config.NodeConfig) []secret {
			var out []secret
			for i := range cfg.Spec.Registries {
				if c := cfg.Spec.Registries[i].Credential; c != nil {
					out = append(out, secret{fmt.Sprintf("spec.registries[%d].credential.password", i), "spec.registries/" + cfg.Spec.Registries[i].Host, &c.Password})
				}
			}
			return out
		},
	},
	{
		own: func(cfg *config.NodeConfig) {
			if h := cfg.Spec.Host; h != nil {
				c := *h
				if h.Orch != nil {
					o := *h.Orch
					c.Orch = &o
				}
				cfg.Spec.Host = &c
			}
		},
		places: func(cfg *config.NodeConfig) []secret {
			if h := cfg.Spec.Host; h != nil && h.Orch != nil {
				return []secret{{"spec.host.orch.enrollKey", "spec.host.orch.enrollKey", &h.Orch.EnrollKey}}
			}
			return nil
		},
	},
	{
		own: func(cfg *config.NodeConfig) {
			if j := cfg.Spec.Join; j != nil {
				c := *j
				cfg.Spec.Join = &c
			}
		},
		places: func(cfg *config.NodeConfig) []secret {
			if j := cfg.Spec.Join; j != nil {
				return []secret{{"spec.join.enrollKey", "spec.join.enrollKey", &j.EnrollKey}}
			}
			return nil
		},
	},
}

// secrets lists every place cfg holds private material, in the table's order.
func secrets(cfg *config.NodeConfig) []secret {
	var out []secret
	for _, k := range secretKinds {
		out = append(out, k.places(cfg)...)
	}
	return out
}

// Redact returns cfg with each non-empty private value replaced by Redacted. A value that is
// empty holds no material and is left empty. cfg is not changed.
func Redact(cfg config.NodeConfig) config.NodeConfig {
	for _, k := range secretKinds {
		k.own(&cfg)
	}
	for _, s := range secrets(&cfg) {
		if *s.value != "" {
			*s.value = Redacted
		}
	}
	return cfg
}

// placeholders returns the fields of next that carry Redacted where stored did not.
func placeholders(next, stored config.NodeConfig) []string {
	was := map[string]bool{}
	for _, s := range secrets(&stored) {
		was[s.id] = *s.value == Redacted
	}
	var out []string
	for _, s := range secrets(&next) {
		if *s.value == Redacted && !was[s.id] {
			out = append(out, s.field)
		}
	}
	return out
}
