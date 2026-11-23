package apply

import (
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/garamsh/naru/lib/init/config"
)

// Redacted is what GET /v1/config shows in place of private material.
const Redacted = "REDACTED"

// shown is the allow-list: every place of a configuration that GET /v1/config shows in clear. A
// place is a string a configuration holds, named by its YAML path with [] for a list element and
// {} for a map value. A string that is not listed here is redacted, so a field the configuration
// gains later is hidden until someone decides it is not private and lists it.
var shown = map[string]bool{
	".apiVersion":                              true,
	".kind":                                    true,
	".metadata.name":                           true,
	".spec.services[].name":                    true,
	".spec.services[].binary":                  true,
	".spec.services[].args[]":                  true,
	".spec.services[].env{}":                   true,
	".spec.services[].keptPair.file":           true,
	".spec.services[].keptPair.organization":   true,
	".spec.services[].restartOnChange[]":       true,
	".spec.services[].logFiles[]":              true,
	".spec.services[].dirs[].path":             true,
	".spec.services[].dirs[].mode":             true,
	".spec.services[].setup[].args[]":          true,
	".spec.services[].setup[].unless[]":        true,
	".spec.services[].restartPolicy":           true,
	".spec.dataDisk.device":                    true,
	".spec.dataDisk.label":                     true,
	".spec.dataDisk.mountPoint":                true,
	".spec.network.forwarding[]":               true,
	".spec.files[].path":                       true,
	".spec.files[].mode":                       true,
	".spec.files[].template":                   true,
	".spec.files[].restart[]":                  true,
	".spec.pki.authorities[].name":             true,
	".spec.pki.authorities[].certificate":      true,
	".spec.pki.authorities[].certificatePath":  true,
	".spec.pki.authorities[].keyPath":          true,
	".spec.pki.certificates[].name":            true,
	".spec.pki.certificates[].authority":       true,
	".spec.pki.certificates[].certificatePath": true,
	".spec.pki.certificates[].keyPath":         true,
	".spec.pki.certificates[].commonName":      true,
	".spec.pki.certificates[].organization":    true,
	".spec.pki.certificates[].usage":           true,
	".spec.pki.certificates[].dnsNames[]":      true,
	".spec.pki.certificates[].ipAddresses[]":   true,
	".spec.pki.certificates[].validity":        true,
	".spec.pki.certificates[].restart[]":       true,
	".spec.time.servers[]":                     true,
	".spec.api.listen":                         true,
	".spec.api.clientCA":                       true,
	".spec.api.certificate":                    true,
	".spec.api.keyFile":                        true,
	".spec.api.revocationPublicKey":            true,
	".spec.api.revocationMaxAge":               true,
	".spec.registries[].host":                  true,
	".spec.registries[].ca":                    true,
	".spec.registries[].credential.username":   true,
	".spec.registries[].credential.helper":     true,
	".spec.aws.region":                         true,
	".spec.aws.trustAnchorArn":                 true,
	".spec.aws.profileArn":                     true,
	".spec.aws.roleArn":                        true,
	".spec.aws.certificate":                    true,
	".spec.aws.key":                            true,
	".spec.storage[].name":                     true,
	".spec.storage[].label":                    true,
	".spec.storage[].device":                   true,
	".spec.storage[].form":                     true,
	".spec.storage[].mountPoint":               true,
	".spec.host.region":                        true,
	".spec.host.labels{}":                      true,
	".spec.host.network.egressInterface":       true,
	".spec.host.orch.url":                      true,
	".spec.host.orch.caSHA256":                 true,
	".spec.host.revocationPublicKey":           true,
	".spec.host.revocationMaxAge":              true,
	".spec.host.revocationList":                true,
	".spec.orch.name":                          true,
	".spec.orch.peerURL":                       true,
	".spec.orch.clientURL":                     true,
	".spec.orch.initialCluster{}":              true,
	".spec.orch.initialClusterState":           true,
	".spec.orch.join":                          true,
	".spec.orch.listen":                        true,
	".spec.orch.sans[]":                        true,
	".spec.orch.certificate":                   true,
	".spec.orch.ca":                            true,
	".spec.orch.revocationPublicKey":           true,
	".spec.orch.hostCA":                        true,
	".spec.orch.recoveryPublicKey":             true,
	".spec.signer.peerListen":                  true,
	".spec.signer.peerNames[]":                 true,
	".spec.signer.bootstrap.recoveryPublicKey": true,
	".spec.signer.bootstrap.rootCSR":           true,
	".spec.signer.replica.join.peer":           true,
	".spec.signer.replica.join.peerCA":         true,
	".spec.signer.replica.join.authorisation":  true,
	".spec.extensions[].name":                  true,
	".spec.extensions[].sha256":                true,
	".spec.metrics.listen":                     true,
	".spec.join.orch.url":                      true,
	".spec.join.orch.caSHA256":                 true,
}

// filesContent is the place of a file's content, which is shown only for a file that group and
// other can read: a file they cannot read holds private material.
const filesContent = ".spec.files[].content"

// secret is one place a configuration holds a string the allow-list does not show.
type secret struct {
	// field names the place for a refusal, with the index of a list element.
	field string
	// id names the place across two configurations: a list element by its key, not its index.
	id string
	// value is the string, settable when the walk was over a configuration of its own.
	value reflect.Value
}

// elementKeys are the YAML names of the field that keys an element of a list, in order of
// preference. An element with none is keyed by its index.
var elementKeys = []string{"name", "host", "path"}

// walk calls visit for each string of v that the allow-list does not show. place is the allow-list
// path of v, field and id are the names of the place for a refusal. A value of a map is copied
// out, visited and put back only when write is set, because a map's values are not addressable.
func walk(v reflect.Value, place, field, id string, write bool, visit func(secret)) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walk(v.Elem(), place, field, id, write, visit)
		}
	case reflect.String:
		if !shown[place] {
			visit(secret{strings.TrimPrefix(field, "."), id, v})
		}
	case reflect.Slice:
		for i := range v.Len() {
			e := v.Index(i)
			k := strconv.Itoa(i)
			if key := keyOf(e); key != "" {
				k = key
			}
			walk(e, place+"[]", field+"["+strconv.Itoa(i)+"]", id+"["+k+"]", write, visit)
		}
	case reflect.Map:
		for _, k := range slices.Sorted(maps(v)) {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(reflect.ValueOf(k)))
			walk(e, place+"{}", field+"."+k, id+"."+k, write, visit)
			if write {
				v.SetMapIndex(reflect.ValueOf(k), e)
			}
		}
	case reflect.Struct:
		if v.Type().PkgPath() != reflect.TypeFor[config.NodeConfig]().PkgPath() {
			return // a value type of another package, such as an address, holds no free text
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name == "-" || !f.IsExported() {
				continue
			}
			p := place + "." + name
			if p == filesContent && v.Type() == reflect.TypeFor[config.File]() && v.Interface().(config.File).Perm()&0o044 != 0 {
				continue
			}
			walk(v.Field(i), p, field+"."+name, id+"."+name, write, visit)
		}
	}
}

func maps(m reflect.Value) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, k := range m.MapKeys() {
			if !yield(k.String()) {
				return
			}
		}
	}
}

// keyOf returns the value of the field that keys a list element, or empty.
func keyOf(e reflect.Value) string {
	if e.Kind() != reflect.Struct || e.Type().PkgPath() != reflect.TypeFor[config.NodeConfig]().PkgPath() {
		return ""
	}
	for _, want := range elementKeys {
		for i := range e.NumField() {
			if strings.Split(e.Type().Field(i).Tag.Get("yaml"), ",")[0] == want && e.Field(i).Kind() == reflect.String {
				return e.Field(i).String()
			}
		}
	}
	return ""
}

// copyOf returns a copy of v that shares no pointer, slice or map with it, down to the values of
// this module's own types; a value of another package is copied by value.
func copyOf(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Type().Elem())
		n.Elem().Set(copyOf(v.Elem()))
		return n
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			n.Index(i).Set(copyOf(v.Index(i)))
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			n.SetMapIndex(it.Key(), copyOf(it.Value()))
		}
		return n
	case reflect.Struct:
		n := reflect.New(v.Type()).Elem()
		n.Set(v)
		if v.Type().PkgPath() == reflect.TypeFor[config.NodeConfig]().PkgPath() {
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					n.Field(i).Set(copyOf(v.Field(i)))
				}
			}
		}
		return n
	}
	return v
}

// secrets lists every place cfg holds a string the allow-list does not show, in document order
// and map keys sorted. The values are cfg's own; a caller that sets one passes a copy.
func secrets(cfg *config.NodeConfig) []secret {
	var out []secret
	walk(reflect.ValueOf(cfg).Elem(), "", "", "", false, func(s secret) { out = append(out, s) })
	return out
}

// Redact returns cfg with each non-empty string the allow-list does not show replaced by
// Redacted. A value that is empty holds no material and is left empty. cfg is not changed.
func Redact(cfg config.NodeConfig) config.NodeConfig {
	out := copyOf(reflect.ValueOf(cfg))
	walk(out, "", "", "", true, func(s secret) {
		if s.value.String() != "" {
			s.value.SetString(Redacted)
		}
	})
	return out.Interface().(config.NodeConfig)
}

// placeholders returns the fields of next that carry Redacted where stored did not.
func placeholders(next, stored config.NodeConfig) []string {
	was := map[string]bool{}
	for _, s := range secrets(&stored) {
		was[s.id] = s.value.String() == Redacted
	}
	var out []string
	for _, s := range secrets(&next) {
		if s.value.String() == Redacted && !was[s.id] {
			out = append(out, s.field)
		}
	}
	return out
}
