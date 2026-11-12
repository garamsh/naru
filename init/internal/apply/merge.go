package apply

import (
	"fmt"
	"slices"
)

// deleteKey marks a keyed element of a patch that removes the element.
const deleteKey = "$patch"

// keyed names the lists a patch merges by key, and the key of each.
var keyed = map[string]string{
	"spec.services":         "name",
	"spec.files":            "path",
	"spec.pki.authorities":  "name",
	"spec.pki.certificates": "name",
}

// merge applies patch to target as a JSON merge patch (RFC 7386) with keyed
// lists, and returns the result. It changes neither argument. field is the
// path of target from the document root.
func merge(target, patch any, field string) (any, error) {
	p, ok := patch.(map[string]any)
	if !ok {
		return clone(patch), nil
	}
	t, _ := target.(map[string]any)
	out := map[string]any{}
	for k, v := range t {
		out[k] = clone(v)
	}
	for k, v := range p {
		path := join(field, k)
		switch list, isList := v.([]any); {
		case v == nil:
			delete(out, k)
		case isList && keyed[path] != "":
			existing, _ := out[k].([]any)
			merged, err := mergeKeyed(existing, list, keyed[path], path)
			if err != nil {
				return nil, err
			}
			out[k] = merged
		default:
			m, err := merge(out[k], v, path)
			if err != nil {
				return nil, err
			}
			out[k] = m
		}
	}
	return out, nil
}

// mergeKeyed merges the elements of patch into existing by key. An element
// with a new key is appended, one with "$patch": "delete" removes its element,
// and any other is merged into the element with the same key.
func mergeKeyed(existing, patch []any, key, field string) ([]any, error) {
	out := slices.Clone(existing)
	find := func(name string) int {
		return slices.IndexFunc(out, func(e any) bool {
			m, _ := e.(map[string]any)
			return m[key] == name
		})
	}
	for i, e := range patch {
		el := fmt.Sprintf("%s[%d]", field, i)
		m, ok := e.(map[string]any)
		if !ok {
			return nil, &InvalidError{Fields: []string{el}, Message: "must be an object"}
		}
		name, ok := m[key].(string)
		if !ok || name == "" {
			return nil, &InvalidError{Fields: []string{el + "." + key}, Message: fmt.Sprintf("must be the %s of the element", key)}
		}
		at := find(name)
		if op, has := m[deleteKey]; has {
			if op != "delete" {
				return nil, &InvalidError{Fields: []string{el + "." + deleteKey}, Message: fmt.Sprintf("must be %q, got %v", "delete", op)}
			}
			if at < 0 {
				return nil, &InvalidError{Fields: []string{el}, Message: fmt.Sprintf("deletes %q, which %s does not hold", name, field)}
			}
			out = slices.Delete(out, at, at+1)
			continue
		}
		if at < 0 {
			merged, err := merge(nil, m, el)
			if err != nil {
				return nil, err
			}
			out = append(out, merged)
			continue
		}
		merged, err := merge(out[at], m, el)
		if err != nil {
			return nil, err
		}
		out[at] = merged
	}
	return out, nil
}

func join(field, k string) string {
	if field == "" {
		return k
	}
	return field + "." + k
}

// clone returns a deep copy of a decoded document.
func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = clone(e)
		}
		return out
	}
	return v
}
