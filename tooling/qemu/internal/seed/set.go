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
	return modify(file, value, path, func(old *yaml.Node, v yaml.Node) error { *old = v; return nil })
}

// Append adds value to the list at path of the user-data in file, making the list when it is missing:
// a service or a file the scenario gives a Node after its first boot.
func Append(file string, value any, path ...string) error {
	return modify(file, value, path, func(old *yaml.Node, v yaml.Node) error {
		if old.Kind != yaml.SequenceNode {
			return errors.New("the field is not a list")
		}
		old.Content = append(old.Content, &v)
		return nil
	})
}

// SetFile sets the content of the spec.files entry of the user-data in file whose path is filePath, which
// has to be there: a scenario that learns after the first boot what a file of gen node's output holds,
// such as the authority naru-hostd trusts for orch's certificate.
func SetFile(file, filePath, content string) error {
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
	spec := field(doc.Content[0], "spec")
	var files *yaml.Node
	if spec != nil && spec.Kind == yaml.MappingNode {
		files = field(spec, "files")
	}
	if files == nil || files.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s has no spec.files", file)
	}
	for _, entry := range files.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if p := field(entry, "path"); p == nil || p.Value != filePath {
			continue
		}
		var v yaml.Node
		if err := v.Encode(content); err != nil {
			return err
		}
		c := field(entry, "content")
		if c == nil {
			entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "content"}, &v)
		} else {
			*c = v
		}
		out, err := yaml.Marshal(&doc)
		if err != nil {
			return err
		}
		return os.WriteFile(file, out, 0o600)
	}
	return fmt.Errorf("%s: spec.files has no entry for %s", file, filePath)
}

// Delete removes the field at path of the user-data in file, and does nothing when it is not there: a
// scenario that boots a Node again without part of its first spec, such as a control host that lost its
// disk and starts without spec.orch.
func Delete(file string, path ...string) error {
	if len(path) == 0 {
		return errors.New("a path is needed")
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
		if m = field(m, key); m == nil || m.Kind != yaml.MappingNode {
			return nil
		}
	}
	last := path[len(path)-1]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == last {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			break
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

// modify finds the node at path, making the maps along it, and has change replace or extend it.
func modify(file string, value any, path []string, change func(old *yaml.Node, v yaml.Node) error) error {
	if len(path) == 0 {
		return errors.New("a path is needed")
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
	old := field(m, last)
	if old == nil {
		// A field that is missing starts as an empty list, which Append extends and Set replaces.
		old = &yaml.Node{Kind: yaml.SequenceNode}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: last}, old)
	}
	if err := change(old, v); err != nil {
		return fmt.Errorf("%s: %s: %w", file, last, err)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}
