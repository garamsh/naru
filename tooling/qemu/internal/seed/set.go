package seed

import (
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// Set sets the field at path of the user-data in file to value, making the maps along the path that
// are missing and keeping every other field and its order. The scenario tests use it for the parts of
// spec that naructl writes no command for: spec.signer, spec.orch, spec.metrics, spec.host.network and
// spec.host.orch (docs/architecture/harness.md).
func Set(file string, value any, path ...string) error {
	if len(path) == 0 {
		return errors.New("Set needs a path")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a user-data mapping", file)
	}
	m := doc.Content[0]
	for _, key := range path[:len(path)-1] {
		next := field(m, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, next)
		}
		if next.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: %s is not a mapping", file, key)
		}
		m = next
	}
	var v yaml.Node
	if err := v.Encode(value); err != nil {
		return err
	}
	last := path[len(path)-1]
	if old := field(m, last); old != nil {
		*old = v
	} else {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: last}, &v)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}
