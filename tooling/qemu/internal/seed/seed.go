// Package seed edits the user-data of a Node that runs as a microVM, for the changes a scenario
// makes that naructl writes no command for.
package seed

import "go.yaml.in/yaml/v3"

func field(m *yaml.Node, key string) *yaml.Node {
	for i := 0; m != nil && i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
